package reproxy

// proxy.go implements the core retry reverse proxy (R5 + R6): the request
// pipeline (parse target -> split query -> parse policy -> capture body ->
// SSRF gates -> attempt loop) and the commit-point guard.

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
// Connection header marks as hop-by-hop), with the forwarded-chain headers
// appended (Via, X-Forwarded-For, X-Forwarded-Proto, X-Forwarded-Host).
func buildOutboundHeaders(src http.Header, r *http.Request) http.Header {
	tokens := connectionTokens(src.Values("Connection"))
	out := http.Header{}
	for name, vals := range src {
		if isHopByHop(name, tokens) {
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
//	parse target -> split query -> parse policy -> capture body ->
//	SSRF gates (allowlist, resolve-then-pin) -> attempt loop ->
//	COMMIT (headers written, body streamed) or exhaustion.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// (1) Target from the path.
	target, rerr := ParsePath(r.URL.EscapedPath())
	if rerr != nil {
		rerr.Write(w)
		return
	}
	if target.Port == 0 {
		target = target.Normalize()
	}

	// (2) Split the query into passthrough + retry params.
	upstreamQuery, retryParams, rerr := SplitQuery(r.URL.RawQuery)
	if rerr != nil {
		rerr.Write(w)
		return
	}

	// (3) Resolve the retry policy (server caps applied inside Parse).
	policy, rerr := Parse(retryParams, p.Config)
	if rerr != nil {
		rerr.Write(w)
		return
	}

	// (4) Capture the request body for replay.
	captured, rerr := Capture(r.Body, p.Config.MaxBody)
	if rerr != nil {
		rerr.Write(w)
		return
	}
	// The request body is NOT closed here: the degraded pass-through path
	// still streams from it (the capture probe consumed only the head).

	degraded := captured.Oversized
	if degraded && p.Config.StrictBodyLimit {
		_ = r.Body.Close()
		(&RequestError{
			Code:   413,
			Reason: fmt.Sprintf("request body exceeds the capture cap of %d bytes, so it cannot be retried", p.Config.MaxBody),
			Hint:   "send a smaller body, or raise --max-body; retries need a buffered copy of the request",
		}).Write(w)
		return
	}
	// Degraded (non-strict) mode: one pass-through attempt, no retry logic.
	// The audit pins streaming pass-through with X-Retry-Dropped and a warn
	// log — the body is never silently dropped.
	if degraded {
		p.logLine("warn", fmt.Sprintf("request body exceeds the %d-byte capture cap: degrading to a single pass-through attempt (no retry)", p.Config.MaxBody))
	}
	attemptsLimit := policy.Default.Attempts
	if degraded {
		attemptsLimit = 1
	}

	// (5) SSRF L2: allowlist gate BEFORE any DNS work.
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
	p.attemptLoop(w, r, target, upstreamQuery, captured, degraded, attemptsLimit, policy, ctx)
}

// attemptLoop drives the retry lifecycle. It is the only writer to the
// ResponseWriter; the commit point is writing the response headers. After
// the commit this function never returns an error to the caller by retrying
// — mid-body failures after commit panic with http.ErrAbortHandler.
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
	// Replay uses the buffered copy. Degraded pass-through streams the
	// original body: the probe's already-read prefix spliced in front of the
	// unread remainder (the capture must not silently eat bytes).
	hadBody := len(captured.Data) > 0 && !captured.Oversized
	degradedBody := degraded && r.Body != nil
	if degradedBody {
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(captured.Prefix), r.Body), r.Body}
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
				p.commitResponse(w, r, resp, cancel, attempts, limit, degraded, true)
				p.logRequest(r, target, resp.StatusCode, attempts, start)
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
		p.commitResponse(w, r, resp, cancel, attempts, limit, degraded, exhausted)
		p.logRequest(r, target, resp.StatusCode, attempts, start)
		return
	}

	// Exhaustion: the loop ran out of attempts (or the budget/gate closed).
	p.exhausted(w, r, target, lastErr, attempts, limit, degraded, start)
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

	// Body replay: fresh reader each attempt. Degraded mode streams the
	// original body instead (prefix + unread remainder), single-shot.
	if captured.Oversized && r.Body != nil {
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
// are attached, WriteHeader is called, and the body is streamed with
// per-write flushing (SSE-friendly). After WriteHeader, retry is impossible
// by construction; mid-body errors abort the client connection via
// panic(http.ErrAbortHandler), mirroring httputil.ReverseProxy.
func (p *Proxy) commitResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, cancel context.CancelFunc, attempts, limit int, degraded, exhausted bool) {
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
	w.Header().Set("X-Retry-Count", strconv.Itoa(attempts))
	w.Header().Set("X-Retry-Limit", strconv.Itoa(limit))
	if degraded {
		w.Header().Set("X-Retry-Dropped", "body-too-large")
	}
	if exhausted {
		w.Header().Set("X-Retry-Exhausted", "1")
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
// time and attempts).
func (p *Proxy) exhausted(
	w http.ResponseWriter,
	r *http.Request,
	target PathTarget,
	lastErr error,
	attempts, limit int,
	degraded bool,
	start time.Time,
) {
	w.Header().Set("X-Retry-Count", strconv.Itoa(attempts))
	w.Header().Set("X-Retry-Limit", strconv.Itoa(limit))
	w.Header().Set("X-Retry-Exhausted", "1")
	if degraded {
		w.Header().Set("X-Retry-Dropped", "body-too-large")
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
	p.logRequest(r, target, 504, attempts, start)
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

// logRequest emits the per-request summary line.
func (p *Proxy) logRequest(r *http.Request, target PathTarget, status int, attempts int, start time.Time) {
	p.logLine("request",
		fmt.Sprintf("method=%s target=%s status=%d attempts=%d duration=%s",
			r.Method, target.HostPort(), status, attempts, time.Since(start).Round(time.Millisecond)))
}

// logLine emits one structured-ish log line via the injectable logger.
func (p *Proxy) logLine(event, msg string) {
	logger := p.Log
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf("event=%s %s", event, msg)
}
