package reproxy

// proxy.go implements the core retry reverse proxy (R5 + R6): the request
// pipeline (parse target + segment policy -> resolve policy carrier -> capture
// body -> SSRF gates -> attempt loop) and the commit-point guard.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Proxy is the top-level http.Handler implementing reproxy. It owns no
// mutable state: the transport and config are shared, per-request state
// lives in the handler call.
type Proxy struct {
	// Config is the server-level configuration (caps, allowlist).
	Config *ServerConfig
	// Transport performs outbound round trips. Injectable for tests; a nil
	// value means "use the PinnedResolver's shared transport" (set via
	// NewProxy, which also wires SSRF resolve-then-pin).
	Transport http.RoundTripper
	// Resolver performs SSRF L3 (resolve-then-pin). Injectable for tests.
	Resolver *PinnedResolver
	// Log receives per-request and per-retry observability lines. Defaults
	// to the stdlib package logger.
	Log *log.Logger
}

// NewProxy builds a Proxy with production defaults: its own PinnedResolver
// and shared transport.
func NewProxy(cfg *ServerConfig) *Proxy {
	pr := NewPinnedResolver()
	return &Proxy{
		Config:    cfg,
		Transport: pr.Transport,
		Resolver:  pr,
		Log:       log.Default(),
	}
}

// hop-by-hop headers stripped in both directions (RFC 9110 section 7.6.1).
// Plus any token named in the Connection header (computed per request).
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// isHopByHop reports whether name is a hop-by-hop header (case-insensitive),
// including tokens advertised in a Connection header value.
func isHopByHop(name string, connectionTokens map[string]bool) bool {
	canonical := http.CanonicalHeaderKey(name)
	for _, h := range hopByHopHeaders {
		if canonical == h {
			return true
		}
	}
	return connectionTokens[strings.ToLower(canonical)]
}

// connectionTokens parses the comma-separated header names carried in a
// Connection header value ("Connection: X-Broken, Keep-Alive" means X-Broken
// and Keep-Alive are hop-by-hop for this connection).
func connectionTokens(values []string) map[string]bool {
	tokens := map[string]bool{}
	for _, v := range values {
		for _, tok := range strings.Split(v, ",") {
			tok = strings.TrimSpace(tok)
			if tok != "" {
				tokens[strings.ToLower(http.CanonicalHeaderKey(tok))] = true
			}
		}
	}
	return tokens
}

// buildOutboundHeaders assembles the upstream request headers from the
// inbound request: all inbound headers minus hop-by-hop ones (plus those the
// Connection header marks as hop-by-hop) and minus the reserved X-Reproxy-*
// namespace (reproxy<->client protocol; the upstream never sees the proxy's
// control plane), with the forwarded-chain headers appended (Via,
// X-Forwarded-For, X-Forwarded-Proto, X-Forwarded-Host).
func buildOutboundHeaders(src http.Header, r *http.Request) http.Header {
	tokens := connectionTokens(src.Values("Connection"))
	out := http.Header{}
	for name, vals := range src {
		if isHopByHop(name, tokens) || isReproxyHeader(name) {
			continue
		}
		for _, v := range vals {
			out.Add(name, v)
		}
	}

	// Via: append this proxy's hop to the existing chain.
	out.Add("Via", viaValue(r))

	// X-Forwarded-For: append the immediate client address.
	if client, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := out.Get("X-Forwarded-For"); prior != "" {
			out.Set("X-Forwarded-For", prior+", "+client)
		} else {
			out.Set("X-Forwarded-For", client)
		}
	}
	out.Set("X-Forwarded-Proto", forwardedProto(r))
	if r.Host != "" {
		out.Set("X-Forwarded-Host", r.Host)
	}
	return out
}

// viaValue renders this proxy's contribution to the Via chain. The inbound
// Via entries are preserved by the header copy; this appends one hop.
func viaValue(r *http.Request) string {
	proto := "1.1"
	if r.ProtoMajor == 2 {
		proto = "2"
	} else if r.ProtoMajor == 1 && r.ProtoMinor == 0 {
		proto = "1.0"
	}
	return proto + " reproxy"
}

// forwardedProto reports the scheme the client used to reach this proxy.
func forwardedProto(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// ServeHTTP implements the request pipeline:
//
//	parse target + optional segment policy -> resolve the policy carrier
//	(segment OR header, never both) -> body (capture only when a retry
//	lifecycle exists) -> SSRF gates (allowlist, resolve-then-pin)
//	-> attempt loop -> COMMIT (headers written, body streamed) or exhaustion.
//
// The query is unconditionally upstream-owned: RawQuery is forwarded
// byte-identical on every path, including retry.*-shaped keys (target data).
// A retry lifecycle exists iff a policy is present — carried in the scheme
// segment (/https+POLICY/host) or the X-Reproxy-Retry-Policy header. No
// policy: single attempt (the literal), body streams, no X-Retry-* headers.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// (1) Target and optional segment policy from the path.
	target, segmentParams, rerr := ParsePath(r.URL.EscapedPath())
	if rerr != nil {
		rerr.Write(w)
		return
	}
	if target.Port == 0 {
		target = target.Normalize()
	}

	// (2) Policy carrier resolution. Exactly one carrier may speak: a
	// segment policy and a header policy together are a 400 (fail closed,
	// never silent precedence). retryLifecycle reports whether a retry
	// lifecycle exists at all (a policy is present); it gates body capture
	// (D14) and the X-Retry-* observability headers.
	//
	// ParseRetryPolicyHeader enforces the reserved X-Reproxy-* namespace on
	// every path (its unknown-member 400 fires here regardless of carrier,
	// so a stray X-Reproxy-* header cannot be smuggled through either way).
	var (
		policy         Policy
		retryLifecycle bool
		policySource   string // request-log field: segment | header | none
	)
	headerParams, rerr := ParseRetryPolicyHeader(r.Header)
	if rerr != nil {
		rerr.Write(w)
		return
	}
	switch {
	case segmentParams != nil && headerParams != nil:
		(&RequestError{
			Code:   400,
			Reason: fmt.Sprintf("both policy channels are in use: the path's scheme segment carries a policy while the %s header carries another", RetryPolicyHeader),
			Hint:   "the scheme-segment channel and the header channel are mutually exclusive: remove the header, or drop the policy from the path (e.g. use /https/host); if you did not set this header, a middleware or gateway between you and reproxy may have added it",
		}).Write(w)
		return
	case segmentParams != nil:
		policy, rerr = Parse(segmentParams, p.Config)
		if rerr != nil {
			rerr.Write(w)
			return
		}
		retryLifecycle = true
		policySource = "segment"
	case headerParams != nil:
		policy, rerr = Parse(headerParams, p.Config)
		if rerr != nil {
			rerr.Write(w)
			return
		}
		retryLifecycle = true
		policySource = "header"
	default:
		// No policy anywhere: the ONLY no-policy spelling. Pure proxy,
		// single attempt — a literal (D13), never Parse(url.Values{}),
		// which would return the v0.1.0 defaults (3 attempts, network=1)
		// and smuggle a retry lifecycle into the policy-less path.
		policy = SingleAttemptPolicy(p.Config)
		policySource = "none"
	}

	// (3) The query belongs to the target: byte-identical pass-through,
	// always (retry.* spellings are target data).
	upstreamQuery := r.URL.RawQuery

	// (4) Body. Capture exists solely to replay across attempts (D14), so it
	// runs only when a retry lifecycle exists (a policy is present). The
	// policy-less path streams the body to the upstream as-is — no cap, no
	// 413, no degraded mode, no X-Retry-Dropped.
	var captured *CapturedBody
	degraded := false
	if retryLifecycle {
		var rerr *RequestError
		captured, rerr = Capture(r.Body, p.Config.MaxBody)
		if rerr != nil {
			rerr.Write(w)
			return
		}
		// The request body is NOT closed here: the degraded pass-through path
		// still streams from it (the capture probe consumed only the head).

		degraded = captured.Oversized
		if degraded && p.Config.StrictBodyLimit {
			_ = r.Body.Close()
			(&RequestError{
				Code:   413,
				Reason: fmt.Sprintf("request body exceeds the capture cap of %d bytes, so it cannot be retried", p.Config.MaxBody),
				Hint:   "send a smaller body, or raise --max-body; retries need a buffered copy of the request",
			}).Write(w)
			return
		}
		// Degraded (non-strict) mode: one pass-through attempt, no retry
		// logic. The audit pins streaming pass-through with X-Retry-Dropped
		// and a warn log — the body is never silently dropped.
		if degraded {
			p.logLine("warn", fmt.Sprintf("request body exceeds the %d-byte capture cap: degrading to a single pass-through attempt (no retry)", p.Config.MaxBody))
		}
	}
	attemptsLimit := policy.Default.Attempts
	if degraded {
		attemptsLimit = 1
	}

	// (5) SSRF L2: allowlist gate BEFORE any DNS work. Identical in every
	// mode: the destination is the same attack surface whether or not
	// retries happen.
	if !p.Config.Allows(target.Host) {
		(&RequestError{
			Code:   403,
			Reason: fmt.Sprintf("upstream host %q is not in the allowlist", target.Host),
			Hint:   "the operator must add the host to --allowlist (or the proxy runs with --dangerous-allow-all)",
		}).Write(w)
		return
	}

	// SSRF L3: resolve-then-pin. The pinned set travels with the request
	// context; the dialer never re-resolves (DNS-rebinding TOCTOU closed).
	pinned, rerr := p.Resolver.ResolveAndValidate(r.Context(), target.Host, strconv.Itoa(target.Port))
	if rerr != nil {
		rerr.Write(w)
		return
	}
	ctx := WithPinnedIPs(r.Context(), pinned)

	// (6) The attempt loop.
	p.attemptLoop(w, r, target, upstreamQuery, captured, degraded, attemptsLimit, policy, ctx, retryLifecycle, policySource)
}

// attemptLoop drives the retry lifecycle. It is the only writer to the
// ResponseWriter; the commit point is writing the response headers. After
// the commit this function never returns an error to the caller by retrying
// — mid-body failures after commit panic with http.ErrAbortHandler.
//
// retryLifecycle gates the X-Retry-* observability headers: a present
// policy (either carrier) has a lifecycle to report; the policy-less path
// does not (single attempt, no gates — nothing to observe). policySource
// names the carrier for the request log line (segment | header | none).
func (p *Proxy) attemptLoop(
	w http.ResponseWriter,
	r *http.Request,
	target PathTarget,
	upstreamQuery string,
	captured *CapturedBody,
	degraded bool,
	attemptsLimit int,
	policy Policy,
	ctx context.Context,
	retryLifecycle bool,
	policySource string,
) {
	start := time.Now()

	// The attempt limit starts at the default scope's attempts; a
	// retry[NNN] scope may raise it once that status actually triggers a
	// retry (the per-status shape applies to the retries it gates). The
	// server cap was already applied inside policy.Parse, so a raise can
	// never exceed what the operator allows.
	limit := attemptsLimit
	if limit < 1 {
		limit = 1
	}

	headers := buildOutboundHeaders(r.Header, r)
	// Replay uses the buffered copy (nil captured = streaming: the body was
	// never captured, so it is forwarded as the request's own stream,
	// single-shot). Degraded pass-through also streams the original body:
	// the probe's already-read prefix spliced in front of the unread
	// remainder (the capture must not silently eat bytes).
	var hadBody bool
	var degradedBody bool
	if captured != nil {
		hadBody = len(captured.Data) > 0 && !captured.Oversized
		degradedBody = degraded && r.Body != nil
		if degradedBody {
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(captured.Prefix), r.Body), r.Body}
		}
	}

	var (
		lastErr    error
		attempts   int
		retryAfter string
	)

	remaining := func() time.Duration {
		left := policy.Budget - time.Since(start)
		if left < 0 {
			return 0
		}
		return left
	}

	for attemptNo := 1; attemptNo <= limit; attemptNo++ {
		// Client gone: abort without responding (nobody is listening).
		if r.Context().Err() != nil {
			return
		}
		// Budget spent before the next attempt starts: stop the lifecycle
		// here (audit item 4: the budget is a hard cap over all attempts and
		// waits). The first attempt is exempt — a request always gets one
		// try at the upstream.
		if attemptNo > 1 && remaining() <= 0 {
			break
		}
		attempts = attemptNo

		resp, cancel, err := p.roundTrip(ctx, r, target, upstreamQuery, headers, captured, hadBody, attemptNo, remaining())

		if err != nil {
			// Network failure (dial/TLS/write/TTFB timeout). The attempt
			// context is already canceled by roundTrip on the error path.
			lastErr = err
			p.logAttempt(target, attemptNo, 0, err.Error(), remaining(), attempts, limit)
			if !policy.NetworkGate || attemptNo == limit {
				break
			}
			wait := ComputeWait(policy.Default, attemptNo, "", remaining(), time.Now())
			if remaining()-wait <= 0 {
				// The budget cannot cover another wait+attempt cycle: the
				// lifecycle ends here with a 504 (no response to deliver;
				// lastErr already names the network failure).
				break
			}
			p.logLine("wait", fmt.Sprintf("target=%s backoff=%s", target.HostPort(), wait.Round(time.Millisecond)))
			if !p.sleepCtx(r.Context(), wait) {
				return
			}
			continue
		}

		// Headers arrived within the TTFB cap.
		if policy.RetryableStatus(resp.StatusCode) && !degraded && attemptNo < limit {
			retryAfter = resp.Header.Get("Retry-After")
			sp := policy.Effective(resp.StatusCode)
			// The per-status scope may raise the attempt limit, but never
			// beyond the server cap; clamp against the resolved limit.
			if sp.Attempts > limit {
				limit = sp.Attempts
			}
			wait := ComputeWait(sp, attemptNo, retryAfter, remaining(), time.Now())
			if remaining()-wait <= 0 {
				// Budget out: the wait would consume the rest of the budget,
				// leaving nothing for another attempt. Deliver the held
				// response (the audit pins "budget exhausted -> return the
				// last response"), flagged as exhausted.
				p.commitResponse(w, r, resp, cancel, attempts, limit, degraded, true, retryLifecycle)
				p.logRequest(r, target, resp.StatusCode, attempts, policySource, start)
				return
			}
			p.drainAndClose(resp)
			cancel()
			p.logAttempt(target, attemptNo, resp.StatusCode, "retryable status", remaining(), attempts, limit)
			p.logLine("wait", fmt.Sprintf("target=%s backoff=%s", target.HostPort(), wait.Round(time.Millisecond)))
			if !p.sleepCtx(r.Context(), wait) {
				return
			}
			continue
		}

		// Not retryable (or last attempt, or degraded single-shot): COMMIT.
		// Every path below writes to w; after the first byte, no retry.
		// A retryable status delivered here means attempts ran out: the
		// client still gets the real upstream verdict, flagged as exhausted.
		exhausted := policy.RetryableStatus(resp.StatusCode) && !degraded
		p.commitResponse(w, r, resp, cancel, attempts, limit, degraded, exhausted, retryLifecycle)
		p.logRequest(r, target, resp.StatusCode, attempts, policySource, start)
		return
	}

	// Exhaustion: the loop ran out of attempts (or the budget/gate closed).
	p.exhausted(w, r, target, lastErr, attempts, limit, degraded, retryLifecycle, policySource, start)
}

// roundTripTimeout caps one attempt's time-to-first-byte: how long the
// proxy waits for upstream response HEADERS (audit item 8: the per-try
// timeout is a TTFB constraint only — never a whole-stream cap, so long
// SSE responses are not killed by it). The per-try cap is bounded by the
// remaining budget so the last attempt cannot outlive the lifecycle.
func (p *Proxy) roundTripTimeout(remaining time.Duration) time.Duration {
	const defaultPerTry = 30 * time.Second
	if remaining <= 0 {
		// Budget spent: fail the attempt immediately rather than falling
		// back to the (much longer) default per-try cap.
		return time.Millisecond
	}
	if remaining < defaultPerTry {
		return remaining
	}
	return defaultPerTry
}

// roundTrip performs one outbound request attempt under a TTFB deadline:
// the attempt is abandoned when response headers do not arrive within the
// per-try cap. Once headers arrive, the timer stops and the body is read
// under the request's own context (idle-watching is a future concern; SSE
// streams are never killed by the retry budget or the TTFB cap).
//
// The cancel function passed in belongs to this attempt's context; callers
// must cancel it after the response body is fully consumed (or failed), so
// in-flight dials for abandoned attempts are reaped.
func (p *Proxy) roundTrip(
	ctx context.Context,
	r *http.Request,
	target PathTarget,
	upstreamQuery string,
	headers http.Header,
	captured *CapturedBody,
	hadBody bool,
	attemptNo int,
	remaining time.Duration,
) (*http.Response, context.CancelFunc, error) {
	_ = attemptNo // waits are computed by the caller; one attempt = one trip

	u := target.URL()
	if upstreamQuery != "" {
		u += "?" + upstreamQuery
	}

	// TTFB context: parented to the pinned-IP context (which is parented
	// to the client request context, so client disconnects abort dials).
	ttfbCtx, cancel := context.WithCancel(ctx)

	req, err := http.NewRequestWithContext(ttfbCtx, r.Method, u, nil)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("reproxy: building outbound request: %w", err)
	}
	req.Host = target.HostPort()
	req.Header = headers

	// Body replay: fresh reader each attempt. The degraded and policy-less
	// paths stream the original body instead (prefix + unread remainder for
	// degraded; the untouched stream for policy-less), single-shot.
	if captured == nil && r.Body != nil {
		// Policy-less streaming: no capture (no retry lifecycle) — the
		// request's own body stream is forwarded once, with the inbound
		// framing preserved (Content-Length stays Content-Length; chunked
		// stays chunked; NoBody stays zero-length).
		req.Body = r.Body
		req.ContentLength = r.ContentLength
	} else if captured != nil && captured.Oversized && r.Body != nil {
		req.Body = r.Body
		req.ContentLength = -1 // chunked: the total size was never read
	} else if hadBody && captured.Reader() != nil {
		req.Body = io.NopCloser(captured.Reader())
		req.ContentLength = captured.Len
	} else {
		req.Body = nil
		req.ContentLength = 0
	}

	type rtResult struct {
		resp *http.Response
		err  error
	}
	done := make(chan rtResult, 1)
	go func() {
		resp, err := p.Transport.RoundTrip(req)
		done <- rtResult{resp: resp, err: err}
	}()

	timer := time.NewTimer(p.roundTripTimeout(remaining))
	defer timer.Stop()

	select {
	case res := <-done:
		if res.err != nil {
			cancel()
			return nil, nil, res.err
		}
		// Headers arrived: the TTFB timer is done. The cancel stays live
		// for the caller (it governs abandoned-attempt reaping and body
		// teardown once the response is consumed).
		return res.resp, cancel, nil
	case <-timer.C:
		// TTFB exceeded: cancel the attempt, then wait for the goroutine to
		// observe it so no result is leaked. If the transport nevertheless
		// delivered a response concurrently with the timer, its body must be
		// closed — otherwise the pooled connection never returns to the pool.
		cancel()
		res := <-done
		p.drainAndClose(res.resp)
		return nil, nil, fmt.Errorf("per-try timeout: no response headers within %s", p.roundTripTimeout(remaining))
	case <-r.Context().Done():
		// Client gone: same reaping as the timeout path.
		cancel()
		res := <-done
		p.drainAndClose(res.resp)
		return nil, nil, r.Context().Err()
	}
}

// commitResponse is the COMMIT point (audit item 8): response headers are
// copied to the client (minus hop-by-hop), X-Retry-* observability headers
// are attached when a retry lifecycle exists, WriteHeader is called, and the
// body is streamed with per-write flushing (SSE-friendly). After
// WriteHeader, retry is impossible by construction; mid-body errors abort
// the client connection via panic(http.ErrAbortHandler), mirroring
// httputil.ReverseProxy.
func (p *Proxy) commitResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, cancel context.CancelFunc, attempts, limit int, degraded, exhausted, retryLifecycle bool) {
	defer p.drainAndClose(resp)
	defer cancel()

	tokens := connectionTokens(resp.Header.Values("Connection"))
	for name, vals := range resp.Header {
		if isHopByHop(name, tokens) {
			continue
		}
		for _, v := range vals {
			w.Header().Add(name, v)
		}
	}
	// Observability headers only when there is a retry lifecycle to report:
	// a present policy (either carrier) has one. The policy-less path emits
	// none (single attempt, no gates, no cap machinery).
	if retryLifecycle {
		w.Header().Set("X-Retry-Count", strconv.Itoa(attempts))
		w.Header().Set("X-Retry-Limit", strconv.Itoa(limit))
		if degraded {
			w.Header().Set("X-Retry-Dropped", "body-too-large")
		}
		if exhausted {
			w.Header().Set("X-Retry-Exhausted", "1")
		}
	}

	w.WriteHeader(resp.StatusCode)

	// Flush immediately after headers so SSE clients see the response start
	// without waiting for the first body chunk, then flush every write.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if _, err := copyWithFlush(w, resp.Body); err != nil && r.Context().Err() == nil {
		// The response has started; the only honest behavior is to abort
		// the client connection (the stream is truncated).
		p.logLine("stream_truncated", fmt.Sprintf("upstream body read failed after commit: %v", err))
		panic(http.ErrAbortHandler)
	}
}

// exhausted handles the attempts-spent path where the final attempt was a
// network failure (no response to deliver): a 504 names the last error.
// Retryable-status exhaustion is handled in the commit path (the last
// upstream response is delivered with X-Retry-Exhausted). Budget exhaustion
// lands here too (waits are budget-capped, so the loop simply runs out of
// time and attempts). The X-Retry-* headers appear only when a retry
// lifecycle exists — the policy-less path fails its single attempt here
// without them (single attempt, no gates: nothing to observe).
func (p *Proxy) exhausted(
	w http.ResponseWriter,
	r *http.Request,
	target PathTarget,
	lastErr error,
	attempts, limit int,
	degraded, retryLifecycle bool,
	policySource string,
	start time.Time,
) {
	if retryLifecycle {
		w.Header().Set("X-Retry-Count", strconv.Itoa(attempts))
		w.Header().Set("X-Retry-Limit", strconv.Itoa(limit))
		w.Header().Set("X-Retry-Exhausted", "1")
		if degraded {
			w.Header().Set("X-Retry-Dropped", "body-too-large")
		}
	}
	reason := "all retry attempts failed with network errors"
	if lastErr != nil {
		reason = fmt.Sprintf("all retry attempts failed; last error: %v", lastErr)
	}
	(&RequestError{
		Code:   504,
		Reason: reason,
		Hint:   "the upstream was unreachable or too slow for every attempt within the retry budget",
	}).Write(w)
	p.logRequest(r, target, 504, attempts, policySource, start)
}

// copyWithFlush streams src to dst, flushing after every write so event
// streams are delivered chunk-by-chunk rather than buffered.
func copyWithFlush(dst io.Writer, src io.Reader) (int64, error) {
	flusher, _ := dst.(http.Flusher)
	total := int64(0)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			written, werr := dst.Write(buf[:n])
			total += int64(written)
			if flusher != nil {
				flusher.Flush()
			}
			if werr != nil {
				return total, werr
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// drainAndClose consumes (bounded) and closes a response body so the
// transport can reuse the connection; bodies are never fully buffered.
func (p *Proxy) drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

// sleepCtx waits for d, returning false if the request context was canceled
// first (client gone: the caller must abort the handler without responding).
func (p *Proxy) sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// logAttempt emits the per-retry observability line.
func (p *Proxy) logAttempt(target PathTarget, attemptNo, status int, reason string, remainingBudget time.Duration, attempts, limit int) {
	p.logLine("retry",
		fmt.Sprintf("target=%s attempt=%d status=%d reason=%q remaining_budget=%s attempts=%d/%d",
			target.HostPort(), attemptNo, status, reason, remainingBudget.Round(time.Millisecond), attempts, limit))
}

// logRequest emits the per-request summary line. policy names the carrier
// the retry policy arrived on (segment | header) or "none" (single-attempt
// pass-through) so operators can measure their channel mix.
func (p *Proxy) logRequest(r *http.Request, target PathTarget, status int, attempts int, policySource string, start time.Time) {
	p.logLine("request",
		fmt.Sprintf("method=%s target=%s policy=%s status=%d attempts=%d duration=%s",
			r.Method, target.HostPort(), policySource, status, attempts, time.Since(start).Round(time.Millisecond)))
}

// logLine emits one structured-ish log line via the injectable logger.
func (p *Proxy) logLine(event, msg string) {
	logger := p.Log
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf("event=%s %s", event, msg)
}
