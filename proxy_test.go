package reproxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockTransport is a scriptable http.RoundTripper for deterministic retry
// tests: each RoundTrip pops the next scripted result (response or error).
type mockTransport struct {
	mu      sync.Mutex
	results []mockResult
	calls   int
	// lastReq captures the most recent outbound request for assertions.
	lastReq *http.Request
}

type mockResult struct {
	resp *http.Response
	err  error
}

func newMockTransport(results ...mockResult) *mockTransport {
	return &mockTransport{results: results}
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastReq = req
	idx := m.calls
	m.calls++
	if idx < len(m.results) {
		r := m.results[idx]
		if r.err != nil {
			return nil, r.err
		}
		return r.resp, nil
	}
	// Beyond the script: fail loudly so overlong retry loops are visible.
	return nil, fmt.Errorf("mock transport: script exhausted at call %d", idx+1)
}

func respFor(status int, body string, hdr http.Header) *http.Response {
	if hdr == nil {
		hdr = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     hdr,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// mockResolverBuilder returns a PinnedResolver whose lookup yields the given
// IPs (public by default so the SSRF layer passes).
func mockResolverBuilder(ips ...string) *PinnedResolver {
	return &PinnedResolver{Lookup: mockLookupBuilder(ips...)}
}

// testProxy builds a Proxy wired for handler-level tests: mock transport and
// a resolver that always pins a public IP for the given host.
func testProxy(transport http.RoundTripper, cfg *ServerConfig) *Proxy {
	if cfg == nil {
		cfg = &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 10 << 20, DangerousAllowAll: true}
	}
	return &Proxy{
		Config:    cfg,
		Transport: transport,
		Resolver:  mockResolverBuilder("93.184.216.10"),
	}
}

// do runs one request through the proxy handler and returns the recorder.
// An http.ErrAbortHandler panic (the post-commit failure mode) is recovered
// so callers can assert on the committed state.
func do(p *Proxy, method, target string, body io.Reader, hdr http.Header) (w *httptest.ResponseRecorder) {
	req := httptest.NewRequest(method, target, body)
	if hdr != nil {
		req.Header = hdr
	}
	w = httptest.NewRecorder()
	defer func() {
		if rec := recover(); rec != nil {
			if err, ok := rec.(error); !ok || err != http.ErrAbortHandler {
				panic(rec)
			}
		}
	}()
	p.ServeHTTP(w, req)
	return w
}

// TestProxyRetriesStatusSequence: 500,500,200 -> success on the third
// attempt; the client sees the 200 body and X-Retry-Count: 3.
func TestProxyRetriesStatusSequence(t *testing.T) {
	mt := newMockTransport(
		mockResult{resp: respFor(500, "boom", nil)},
		mockResult{resp: respFor(500, "boom", nil)},
		mockResult{resp: respFor(200, "hello", nil)},
	)
	p := testProxy(mt, nil)

	w := do(p, "GET", "/http+status=500;*.initial=1ms;*.max=2ms;*.jitter=none/up.example.com/x", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "hello" {
		t.Errorf("body = %q, want %q", got, "hello")
	}
	if got := w.Header().Get("X-Retry-Count"); got != "3" {
		t.Errorf("X-Retry-Count = %q, want 3", got)
	}
	if got := w.Header().Get("X-Retry-Limit"); got != "3" {
		t.Errorf("X-Retry-Limit = %q, want 3", got)
	}
	if mt.calls != 3 {
		t.Errorf("transport calls = %d, want 3", mt.calls)
	}
}

// TestProxyNoRetryWithoutStatusGate: a policy with shaping but no status gate
// never retries — 500 relays to the client on the first attempt
// (gate-and-shape separation).
func TestProxyNoRetryWithoutStatusGate(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(500, "boom", nil)})
	p := testProxy(mt, nil)

	w := do(p, "GET", "/http+*.attempts=3/up.example.com/x", nil, nil)
	if w.Code != 500 {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if mt.calls != 1 {
		t.Errorf("transport calls = %d, want 1 (no status gate -> no retry)", mt.calls)
	}
	if got := w.Header().Get("X-Retry-Count"); got != "1" {
		t.Errorf("X-Retry-Count = %q, want 1", got)
	}
}

// TestProxyNetworkErrorRetry: dial error then success -> retry via the
// network gate, client gets the good response.
func TestProxyNetworkErrorRetry(t *testing.T) {
	mt := newMockTransport(
		mockResult{err: fmt.Errorf("dial tcp: connection refused")},
		mockResult{resp: respFor(200, "ok", nil)},
	)
	p := testProxy(mt, nil)

	w := do(p, "GET", "/http+network=1;*.initial=1ms;*.max=2ms;*.jitter=none/up.example.com/x", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if mt.calls != 2 {
		t.Errorf("transport calls = %d, want 2", mt.calls)
	}
}

// TestProxyNetworkGateClosed: network=0 -> a network error exhausts
// immediately with a 504 and no further attempts.
func TestProxyNetworkGateClosed(t *testing.T) {
	mt := newMockTransport(mockResult{err: fmt.Errorf("dial tcp: connection refused")})
	p := testProxy(mt, nil)

	w := do(p, "GET", "/http+network=0/up.example.com/x", nil, nil)
	if w.Code != 504 {
		t.Fatalf("status = %d, want 504", w.Code)
	}
	if mt.calls != 1 {
		t.Errorf("transport calls = %d, want 1", mt.calls)
	}
	if got := w.Header().Get("X-Retry-Exhausted"); got != "1" {
		t.Errorf("X-Retry-Exhausted = %q, want 1", got)
	}
}

// TestProxyExhaustedDeliversLastResponse: attempts exhausted on retryable
// statuses -> the LAST upstream response is delivered (real verdict).
func TestProxyExhaustedDeliversLastResponse(t *testing.T) {
	mt := newMockTransport(
		mockResult{resp: respFor(503, "try later 1", nil)},
		mockResult{resp: respFor(503, "try later 2", nil)},
		mockResult{resp: respFor(503, "try later 3", nil)},
	)
	p := testProxy(mt, nil)

	w := do(p, "GET", "/http+status=503;*.attempts=3;*.initial=1ms;*.max=2ms;*.jitter=none/up.example.com/x", nil, nil)
	if w.Code != 503 {
		t.Fatalf("status = %d, want 503 (last response delivered)", w.Code)
	}
	if got := w.Body.String(); got != "try later 3" {
		t.Errorf("body = %q, want the last attempt's body", got)
	}
	if got := w.Header().Get("X-Retry-Count"); got != "3" {
		t.Errorf("X-Retry-Count = %q, want 3", got)
	}
	if got := w.Header().Get("X-Retry-Exhausted"); got != "1" {
		t.Errorf("X-Retry-Exhausted = %q, want 1", got)
	}
	if mt.calls != 3 {
		t.Errorf("transport calls = %d, want 3", mt.calls)
	}
}

// TestProxyNetworkExhaustion504: all attempts fail on the network -> 504
// with X-Retry-Exhausted and no body from an upstream.
func TestProxyNetworkExhaustion504(t *testing.T) {
	mt := newMockTransport(
		mockResult{err: fmt.Errorf("dial tcp: connection refused (1)")},
		mockResult{err: fmt.Errorf("dial tcp: connection refused (2)")},
	)
	p := testProxy(mt, nil)

	w := do(p, "GET", "/http+*.attempts=2;*.initial=1ms;*.max=2ms;*.jitter=none/up.example.com/x", nil, nil)
	if w.Code != 504 {
		t.Fatalf("status = %d, want 504", w.Code)
	}
	if got := w.Header().Get("X-Retry-Exhausted"); got != "1" {
		t.Errorf("X-Retry-Exhausted = %q, want 1", got)
	}
	if !strings.Contains(w.Body.String(), "connection refused") {
		t.Errorf("504 body should name the last error, got %q", w.Body.String())
	}
}

// TestProxyBudgetExhaustion: a tiny budget with a long wait -> the wait is
// capped to the remaining budget and the request ends in 504 (no upstream
// ever succeeded).
func TestProxyBudgetExhaustion(t *testing.T) {
	mt := newMockTransport(
		mockResult{err: fmt.Errorf("dial tcp: connection refused")},
		mockResult{err: fmt.Errorf("dial tcp: connection refused")},
	)
	cfg := &ServerConfig{MaxAttempts: 10, MaxBudget: 100 * time.Millisecond, MaxBody: 10 << 20, DangerousAllowAll: true}
	p := testProxy(mt, cfg)

	start := time.Now()
	w := do(p, "GET", "/http+*.initial=30s;*.max=60s;*.jitter=none/up.example.com/x", nil, nil)
	elapsed := time.Since(start)
	if w.Code != 504 {
		t.Fatalf("status = %d, want 504", w.Code)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("budget should cap the wait; elapsed %s exceeds the 100ms budget", elapsed)
	}
}

// TestProxySSRFAllowlistGate: host not in the allowlist -> 403 before any
// DNS or transport work.
func TestProxySSRFAllowlistGate(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "should not happen", nil)})
	resolver := mockResolverBuilder("93.184.216.10")
	cfg := &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 10 << 20, Allowlist: []string{"allowed.example.com"}}
	p := &Proxy{Config: cfg, Transport: mt, Resolver: resolver}

	w := do(p, "GET", "/http/other.example.com/x", nil, nil)
	if w.Code != 403 {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "allowlist") {
		t.Errorf("403 body should mention the allowlist: %q", w.Body.String())
	}
	if mt.calls != 0 {
		t.Errorf("transport calls = %d, want 0 (allowlist gate precedes all upstream work)", mt.calls)
	}
}

// TestProxySSRFPrivateResolutionRejected: a resolver yielding a private IP
// -> 403, no transport calls.
func TestProxySSRFPrivateResolutionRejected(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "should not happen", nil)})
	p := &Proxy{
		Config:    &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 10 << 20, DangerousAllowAll: true},
		Transport: mt,
		Resolver:  mockResolverBuilder("10.0.0.1"),
	}

	w := do(p, "GET", "/http/intranet.example.com/x", nil, nil)
	if w.Code != 403 {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if mt.calls != 0 {
		t.Errorf("transport calls = %d, want 0", mt.calls)
	}
}

// TestProxySSRFIPLiteralPrivate: an IP-literal private target -> 403 with
// zero DNS lookups and zero transport calls.
func TestProxySSRFIPLiteralPrivate(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "no", nil)})
	lookupCalls := 0
	resolver := &PinnedResolver{
		Lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			lookupCalls++
			return mockLookupBuilder("93.184.216.10")(ctx, host)
		},
	}
	p := &Proxy{
		Config:    &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 10 << 20, DangerousAllowAll: true},
		Transport: mt,
		Resolver:  resolver,
	}

	w := do(p, "GET", "/http/10.1.2.3/x", nil, nil)
	if w.Code != 403 {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if lookupCalls != 0 {
		t.Errorf("DNS lookups = %d, want 0 for IP-literal target", lookupCalls)
	}
	if mt.calls != 0 {
		t.Errorf("transport calls = %d, want 0", mt.calls)
	}
}

// TestProxyHopByHopStrippedOutbound: hop-by-hop headers (plus Connection
// tokens) never reach the upstream; Via / X-Forwarded-* are added.
func TestProxyHopByHopStrippedOutbound(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "ok", nil)})
	p := testProxy(mt, nil)

	inbound := http.Header{}
	inbound.Set("Connection", "keep-alive, X-Broken-Token")
	inbound.Set("Keep-Alive", "timeout=5")
	inbound.Set("TE", "trailers")
	inbound.Set("X-Broken-Token", "hop-scoped")
	inbound.Set("X-Keep", "client data")

	w := do(p, "GET", "/http/up.example.com/x", nil, inbound)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	req := mt.lastReq
	if req == nil {
		t.Fatal("no outbound request captured")
	}
	for _, h := range []string{"Connection", "Keep-Alive", "TE", "X-Broken-Token"} {
		if _, ok := req.Header[h]; ok {
			t.Errorf("hop-by-hop header %q forwarded upstream", h)
		}
	}
	if req.Header.Get("X-Keep") != "client data" {
		t.Errorf("X-Keep not preserved: %v", req.Header["X-Keep"])
	}
	if req.Header.Get("Via") != "1.1 reproxy" {
		t.Errorf("Via = %q, want \"1.1 reproxy\"", req.Header.Get("Via"))
	}
	if !strings.Contains(req.Header.Get("X-Forwarded-For"), "192.0.2.1") {
		t.Errorf("X-Forwarded-For = %q, want client address appended", req.Header.Get("X-Forwarded-For"))
	}
	if req.Header.Get("X-Forwarded-Proto") != "http" {
		t.Errorf("X-Forwarded-Proto = %q, want http", req.Header.Get("X-Forwarded-Proto"))
	}
}

// TestProxyHopByHopStrippedInbound: hop-by-hop response headers never reach
// the client; end-to-end headers do.
func TestProxyHopByHopStrippedInbound(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("Connection", "close")
	hdr.Set("Keep-Alive", "timeout=5")
	hdr.Set("Transfer-Encoding", "chunked")
	hdr.Set("X-Upstream-Data", "kept")
	mt := newMockTransport(mockResult{resp: respFor(200, "ok", hdr)})
	p := testProxy(mt, nil)

	w := do(p, "GET", "/http/up.example.com/x", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if c := w.Header().Get("Connection"); c == "close" {
		t.Error("Connection header forwarded to client")
	}
	if w.Header().Get("Keep-Alive") != "" {
		t.Error("Keep-Alive header forwarded to client")
	}
	if w.Header().Get("X-Upstream-Data") != "kept" {
		t.Errorf("end-to-end header dropped: %v", w.Header())
	}
}

// TestProxyQueryBytePreservation: the passthrough query round-trips
// byte-identically (signed-URL safety, PRD acceptance item).
func TestProxyQueryBytePreservation(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "ok", nil)})
	p := testProxy(mt, nil)

	raw := "a=%2Fpath%20with%20space&b=plus+sign&c&d=&e=1&e=2&f=%E4%B8%AD"
	w := do(p, "GET", "/http+status=500;*.attempts=2/up.example.com/p?"+raw, nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := mt.lastReq.URL.RawQuery
	want := "a=%2Fpath%20with%20space&b=plus+sign&c&d=&e=1&e=2&f=%E4%B8%AD"
	if got != want {
		t.Errorf("upstream query = %q, want %q (byte-identical)", got, want)
	}
}

// TestProxyDegradedBodyPassthrough: an oversized body in non-strict mode is
// forwarded once with X-Retry-Dropped and no retry (attempt semantics).
func TestProxyDegradedBodyPassthrough(t *testing.T) {
	// The mock transport streams a body of its own; the request body is
	// oversized so reproxy must degrade to a single pass-through attempt.
	mt := newMockTransport(
		mockResult{resp: respFor(500, "upstream says no", nil)},
		mockResult{resp: respFor(500, "never retried", nil)},
	)
	cfg := &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 8, DangerousAllowAll: true}
	p := testProxy(mt, cfg)

	w := do(p, "POST", "/http+status=500;*.attempts=3/up.example.com/x", strings.NewReader("0123456789"), nil)
	if w.Code != 500 {
		t.Fatalf("status = %d, want 500 (single pass-through)", w.Code)
	}
	if got := w.Header().Get("X-Retry-Dropped"); got != "body-too-large" {
		t.Errorf("X-Retry-Dropped = %q, want body-too-large", got)
	}
	if got := w.Header().Get("X-Retry-Count"); got != "1" {
		t.Errorf("X-Retry-Count = %q, want 1 (degraded = no retry)", got)
	}
	if mt.calls != 1 {
		t.Errorf("transport calls = %d, want 1", mt.calls)
	}
}

// TestProxyDegradedBodyStillSSRFChecked: degraded mode does not skip the
// SSRF gates (private resolution still 403).
func TestProxyDegradedBodyStillSSRFChecked(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "no", nil)})
	cfg := &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 4, DangerousAllowAll: true}
	p := &Proxy{
		Config:    cfg,
		Transport: mt,
		Resolver:  mockResolverBuilder("10.0.0.1"),
	}

	w := do(p, "POST", "/http/up.example.com/x", strings.NewReader("0123456789"), nil)
	if w.Code != 403 {
		t.Fatalf("status = %d, want 403 (SSRF unconditional)", w.Code)
	}
	if mt.calls != 0 {
		t.Errorf("transport calls = %d, want 0", mt.calls)
	}
}

// TestProxyStrictBodyLimit413: oversized body in strict mode under a policy
// (capture exists to replay) -> 413.
func TestProxyStrictBodyLimit413(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "no", nil)})
	cfg := &ServerConfig{
		MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 4,
		StrictBodyLimit: true, DangerousAllowAll: true,
	}
	p := testProxy(mt, cfg)

	w := do(p, "POST", "/http+status=5xx/up.example.com/x", strings.NewReader("0123456789"), nil)
	if w.Code != 413 {
		t.Fatalf("status = %d, want 413", w.Code)
	}
	if mt.calls != 0 {
		t.Errorf("transport calls = %d, want 0", mt.calls)
	}
	if !strings.Contains(w.Body.String(), "4") {
		t.Errorf("413 body should mention the cap: %q", w.Body.String())
	}
}

// TestProxyBodyReplayedAcrossRetries: a captured body is replayed verbatim
// on every attempt.
func TestProxyBodyReplayedAcrossRetries(t *testing.T) {
	mt := &mockTransport{
		results: []mockResult{
			{resp: respFor(500, "boom", nil)},
			{resp: respFor(500, "boom", nil)},
			{resp: respFor(200, "ok", nil)},
		},
	}
	// Use a recording transport wrapper so bodies are captured without
	// disturbing the mock script.
	rt := &bodyRecordingTransport{inner: mt}
	p := testProxy(rt, nil)

	w := do(p, "POST", "/http+status=500;*.initial=1ms;*.jitter=none/up.example.com/x", strings.NewReader("payload-123"), nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(rt.bodies) != 3 {
		t.Fatalf("attempts recorded = %d, want 3", len(rt.bodies))
	}
	for i, b := range rt.bodies {
		if b != "payload-123" {
			t.Errorf("attempt %d body = %q, want %q (replay)", i+1, b, "payload-123")
		}
	}
}

// bodyRecordingTransport records request bodies across attempts.
type bodyRecordingTransport struct {
	inner  http.RoundTripper
	bodies []string
}

func (b *bodyRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		data, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		b.bodies = append(b.bodies, string(data))
		// Re-body so the inner transport can read it.
		req.Body = io.NopCloser(strings.NewReader(string(data)))
		req.ContentLength = int64(len(data))
	}
	return b.inner.RoundTrip(req)
}

// TestProxyRetryAfterHonored: a 429 with Retry-After: 1 drives the next
// attempt no earlier than ~1s later.
func TestProxyRetryAfterHonored(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("Retry-After", "1")
	mt := newMockTransport(
		mockResult{resp: respFor(429, "slow down", hdr)},
		mockResult{resp: respFor(200, "ok", nil)},
	)
	p := testProxy(mt, nil)

	start := time.Now()
	w := do(p, "GET", "/http+status=429;*.retry_after=honor;*.jitter=none/up.example.com/x", nil, nil)
	elapsed := time.Since(start)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if mt.calls != 2 {
		t.Errorf("transport calls = %d, want 2", mt.calls)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("Retry-After: 1 should delay the retry ~1s; elapsed only %s", elapsed)
	}
}

// TestProxyRetryAfterCappedByBudget: a huge Retry-After is capped to the
// remaining budget, so the request cannot hang.
func TestProxyRetryAfterCappedByBudget(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("Retry-After", "3600")
	mt := newMockTransport(
		mockResult{resp: respFor(429, "slow down", hdr)},
		mockResult{resp: respFor(429, "still slow", hdr)},
		mockResult{resp: respFor(429, "still slow", hdr)},
	)
	cfg := &ServerConfig{MaxAttempts: 10, MaxBudget: 150 * time.Millisecond, MaxBody: 10 << 20, DangerousAllowAll: true}
	p := testProxy(mt, cfg)

	start := time.Now()
	w := do(p, "GET", "/http+status=429;*.retry_after=honor/up.example.com/x", nil, nil)
	elapsed := time.Since(start)
	if w.Code != 429 {
		t.Fatalf("status = %d, want 429 (exhausted, last response delivered)", w.Code)
	}
	if got := w.Header().Get("X-Retry-Exhausted"); got != "1" {
		t.Errorf("X-Retry-Exhausted = %q, want 1", got)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Retry-After: 3600 must be capped by the budget; elapsed %s", elapsed)
	}
}

// TestProxyRetryAfterIgnored: retry_after=ignore skips the header entirely.
func TestProxyRetryAfterIgnored(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("Retry-After", "5")
	mt := newMockTransport(
		mockResult{resp: respFor(429, "slow down", hdr)},
		mockResult{resp: respFor(200, "ok", nil)},
	)
	p := testProxy(mt, nil)

	start := time.Now()
	w := do(p, "GET", "/http+status=429;*.retry_after=ignore;*.initial=1ms;*.max=2ms;*.jitter=none/up.example.com/x", nil, nil)
	elapsed := time.Since(start)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// Generous upper bound: the point is that a 5s Retry-After was NOT
	// honored (the wait would be seconds), not that the pipeline is fast.
	// Windows timer/scheduler jitter alone spans tens of milliseconds.
	if elapsed > 500*time.Millisecond {
		t.Errorf("ignored Retry-After must not delay; elapsed %s", elapsed)
	}
}

// TestProxyDeadConfig400: an NNN.FIELD scope for a status not in the gate is
// a 400 (fail closed), not a silent pass-through.
func TestProxyDeadConfig400(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "no", nil)})
	p := testProxy(mt, nil)

	w := do(p, "GET", "/http+status=500;429.attempts=4/up.example.com/x", nil, nil)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 (dead config)", w.Code)
	}
	if mt.calls != 0 {
		t.Errorf("transport calls = %d, want 0", mt.calls)
	}
	if !strings.Contains(w.Body.String(), "retry[429]") {
		t.Errorf("400 body should name the dead scope: %q", w.Body.String())
	}
}

// TestProxyUnknownRetryKey400: an unknown policy key is a 400 naming the key.
func TestProxyUnknownRetryKey400(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "no", nil)})
	p := testProxy(mt, nil)

	w := do(p, "GET", "/http+wat=1/up.example.com/x", nil, nil)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if mt.calls != 0 {
		t.Errorf("transport calls = %d, want 0", mt.calls)
	}
	if !strings.Contains(w.Body.String(), "wat") {
		t.Errorf("400 body should name the key: %q", w.Body.String())
	}
}

// TestProxyBadTarget400: a malformed target is a 400 naming the cause.
func TestProxyBadTarget400(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "no", nil)})
	p := testProxy(mt, nil)

	for _, tc := range []struct{ path, wantIn string }{
		{"/ftp/up.example.com/x", "scheme"},
		{"/http/user:pass@up.example.com/x", "userinfo"},
		{"/http/up.example.com:08080/x", "port"},
		{"/", "target"},
	} {
		w := do(p, "GET", tc.path, nil, nil)
		if w.Code != 400 {
			t.Errorf("%s: status = %d, want 400", tc.path, w.Code)
		}
		if !strings.Contains(w.Body.String(), tc.wantIn) {
			t.Errorf("%s: body %q should mention %q", tc.path, w.Body.String(), tc.wantIn)
		}
	}
	if mt.calls != 0 {
		t.Errorf("transport calls = %d, want 0", mt.calls)
	}
}

// TestProxyRedirectNotFollowed: a 3xx from the upstream is relayed as-is
// (never followed; SSRF layer L5).
func TestProxyRedirectNotFollowed(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("Location", "http://10.0.0.1/secret")
	mt := newMockTransport(mockResult{resp: respFor(302, "", hdr)})
	p := testProxy(mt, nil)

	w := do(p, "GET", "/http/up.example.com/x", nil, nil)
	if w.Code != 302 {
		t.Fatalf("status = %d, want 302 (relayed, not followed)", w.Code)
	}
	if got := w.Header().Get("Location"); got != "http://10.0.0.1/secret" {
		t.Errorf("Location = %q, want relayed verbatim", got)
	}
	if mt.calls != 1 {
		t.Errorf("transport calls = %d, want 1 (no follow-up)", mt.calls)
	}
}

// TestProxyCommitPointNoRetryAfterHeaders: once headers are written, an
// upstream death mid-body triggers NO retry — the client sees the truncated
// response and the upstream is called exactly once (PRD acceptance item).
func TestProxyCommitPointNoRetryAfterHeaders(t *testing.T) {
	mt := newMockTransport(mockResult{})
	mt.results = []mockResult{{resp: &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       io.NopCloser(&errAfterReader{prefix: "partial data "}),
	}}}
	p := testProxy(mt, nil)

	// The handler panics with http.ErrAbortHandler after the truncated
	// write (mirroring httputil.ReverseProxy); do() recovers it so the
	// recorder's committed state can be inspected. 200 is NOT in the gate:
	// headers commit immediately, then the body read fails mid-stream.
	w := do(p, "GET", "/http+status=500;*.attempts=3;*.initial=1ms;*.jitter=none/up.example.com/x", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (already committed)", w.Code)
	}
	if got := w.Body.String(); got != "partial data " {
		t.Errorf("body = %q, want the partial bytes", got)
	}
	if mt.calls != 1 {
		t.Errorf("transport calls = %d, want 1 (NO retry after commit)", mt.calls)
	}
	if got := w.Header().Get("X-Retry-Count"); got != "1" {
		t.Errorf("X-Retry-Count = %q, want 1", got)
	}
}

// errAfterReader yields prefix bytes on first Read then errors on every
// subsequent read (an upstream that dies mid-body).
type errAfterReader struct {
	prefix string
	done   bool
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	if !e.done {
		e.done = true
		return copy(p, e.prefix), nil
	}
	return 0, fmt.Errorf("upstream connection reset mid-body")
}

// TestProxySSEStreamedNotBuffered: an SSE-style upstream response is
// delivered as chunks arrive, not buffered until the stream ends. The
// writes are timestamped; a streamed response shows spread-out writes.
func TestProxySSEStreamedNotBuffered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		for _, ev := range []string{"data: one\n\n", "data: two\n\n", "data: three\n\n"} {
			_, _ = w.Write([]byte(ev))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(60 * time.Millisecond)
		}
	}))
	defer srv.Close()

	// Real-transport integration: the resolver validates a public IP (the
	// SSRF layer must pass), while the dial redirects to the local test
	// listener. This exercises the full production path: shared transport,
	// pinned DialContext, streaming copy.
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	pr := &PinnedResolver{
		Lookup: mockLookupBuilder("93.184.216.10"),
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(host, port))
		},
	}
	pr.Transport = &http.Transport{
		Proxy:       nil,
		DialContext: pr.DialContext,
	}
	p := &Proxy{
		Config:    &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 10 << 20, DangerousAllowAll: true},
		Transport: pr.Transport,
		Resolver:  pr,
	}

	target := "/http/up.example.com/stream"
	req := httptest.NewRequest("GET", target, nil)
	rec := newChunkTimingRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(rec.chunks) < 3 {
		t.Fatalf("writes observed = %d, want >= 3 (one per event)", len(rec.chunks))
	}
	// Streamed: the first write lands well before the last one (3 events,
	// 60ms apart). A buffered-until-end response would show all writes
	// within a couple milliseconds.
	spread := rec.chunks[len(rec.chunks)-1].Sub(rec.chunks[0])
	if spread < 100*time.Millisecond {
		t.Errorf("write spread = %s, want >= 100ms (chunks must be delivered as they arrive)", spread)
	}
	if !strings.Contains(rec.Body.String(), "data: three") {
		t.Errorf("body should contain all events, got %q", rec.Body.String())
	}
}

// chunkTimingRecorder wraps ResponseRecorder to timestamp each Write.
type chunkTimingRecorder struct {
	*httptest.ResponseRecorder
	chunks []time.Time
}

func newChunkTimingRecorder() *chunkTimingRecorder {
	return &chunkTimingRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (c *chunkTimingRecorder) Write(p []byte) (int, error) {
	c.chunks = append(c.chunks, time.Now())
	return c.ResponseRecorder.Write(p)
}

// Flush satisfies http.Flusher so the copy loop's per-write flush path is
// exercised; the underlying recorder's Flush records the flag.
func (c *chunkTimingRecorder) Flush() {
	c.ResponseRecorder.Flush()
}

// TestProxyIntegrationRetryAgainstRealUpstream: full-stack integration — a
// real HTTP server that fails twice then succeeds, reached through the real
// transport and pinned dialer (no mocks above the network layer).
func TestProxyIntegrationRetryAgainstRealUpstream(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("boom"))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("finally"))
	}))
	defer srv.Close()

	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	pr := &PinnedResolver{
		Lookup: mockLookupBuilder("93.184.216.10"),
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(host, port))
		},
	}
	pr.Transport = &http.Transport{
		Proxy:       nil,
		DialContext: pr.DialContext,
	}
	p := &Proxy{
		Config:    &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 10 << 20, DangerousAllowAll: true},
		Transport: pr.Transport,
		Resolver:  pr,
	}

	w := do(p, "GET", "/http+status=500;*.initial=5ms;*.max=10ms;*.jitter=none/up.example.com/x", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (third attempt)", w.Code)
	}
	if got := w.Body.String(); got != "finally" {
		t.Errorf("body = %q, want %q", got, "finally")
	}
	if got := w.Header().Get("X-Retry-Count"); got != "3" {
		t.Errorf("X-Retry-Count = %q, want 3", got)
	}
}

// TestProxyTTFBTimeoutRetryable: an upstream that accepts but never writes
// (TTFB hang) is a retryable network failure; repeated hangs exhaust into a
// 504. The per-try timeout is driven by the retry budget here.
func TestProxyTTFBTimeoutRetryable(t *testing.T) {
	// Listener that accepts connections and never writes anything back.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open, never writing (TTFB hang).
			go func(c net.Conn) {
				time.Sleep(2 * time.Second)
				_ = c.Close()
			}(c)
		}
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	pr := &PinnedResolver{
		Lookup: mockLookupBuilder("93.184.216.10"),
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(host, port))
		},
	}
	pr.Transport = &http.Transport{
		Proxy:       nil,
		DialContext: pr.DialContext,
	}
	// A tiny budget makes each attempt's TTFB wait bounded: the budget
	// context wraps the round trips.
	cfg := &ServerConfig{MaxAttempts: 10, MaxBudget: 300 * time.Millisecond, MaxBody: 10 << 20, DangerousAllowAll: true}
	p := &Proxy{
		Config:    cfg,
		Transport: pr.Transport,
		Resolver:  pr,
	}

	start := time.Now()
	w := do(p, "GET", "/http+*.initial=5ms;*.max=10ms;*.jitter=none/up.example.com/x", nil, nil)
	elapsed := time.Since(start)
	if w.Code != 504 {
		t.Fatalf("status = %d, want 504 (TTFB hangs exhausted)", w.Code)
	}
	if got := w.Header().Get("X-Retry-Exhausted"); got != "1" {
		t.Errorf("X-Retry-Exhausted = %q, want 1", got)
	}
	if elapsed > 2*time.Second {
		t.Errorf("hang should be bounded by the budget; elapsed %s", elapsed)
	}
}

// TestProxyRetryAfterHTTPDate: the HTTP-date form of Retry-After is honored.
// HTTP-dates have one-second granularity, so the target date is 2s out and
// the retry must land at or after ~1s of real delay.
func TestProxyRetryAfterHTTPDate(t *testing.T) {
	hdr := http.Header{}
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	hdr.Set("Retry-After", future)
	mt := newMockTransport(
		mockResult{resp: respFor(429, "slow down", hdr)},
		mockResult{resp: respFor(200, "ok", nil)},
	)
	p := testProxy(mt, nil)

	start := time.Now()
	w := do(p, "GET", "/http+status=429;*.retry_after=honor/up.example.com/x", nil, nil)
	elapsed := time.Since(start)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("HTTP-date Retry-After (2s out, truncated to second granularity) should delay the retry; elapsed %s", elapsed)
	}
}

// TestProxyRetryAfterPastDate: a Retry-After date in the past means zero
// delay (retry immediately).
func TestProxyRetryAfterPastDate(t *testing.T) {
	hdr := http.Header{}
	past := time.Now().Add(-1 * time.Hour).UTC().Format(http.TimeFormat)
	hdr.Set("Retry-After", past)
	mt := newMockTransport(
		mockResult{resp: respFor(429, "slow down", hdr)},
		mockResult{resp: respFor(200, "ok", nil)},
	)
	p := testProxy(mt, nil)

	start := time.Now()
	w := do(p, "GET", "/http+status=429;*.retry_after=honor/up.example.com/x", nil, nil)
	elapsed := time.Since(start)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("past Retry-After must mean zero delay; elapsed %s", elapsed)
	}
}

// TestProxyViaAndXFFChainAppend: existing Via / X-Forwarded-For entries are
// preserved and appended to, not replaced.
func TestProxyViaAndXFFChainAppend(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "ok", nil)})
	p := testProxy(mt, nil)

	inbound := http.Header{}
	inbound.Set("Via", "1.0 earlier-proxy")
	inbound.Set("X-Forwarded-For", "198.51.100.7")

	w := do(p, "GET", "/http/up.example.com/x", nil, inbound)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	via := mt.lastReq.Header.Values("Via")
	if len(via) != 2 || via[0] != "1.0 earlier-proxy" || via[1] != "1.1 reproxy" {
		t.Errorf("Via chain = %v, want [1.0 earlier-proxy, 1.1 reproxy]", via)
	}
	xff := mt.lastReq.Header.Get("X-Forwarded-For")
	if !strings.HasPrefix(xff, "198.51.100.7, ") {
		t.Errorf("X-Forwarded-For = %q, want the prior chain preserved then appended", xff)
	}
	if !strings.Contains(xff, "192.0.2.1") {
		t.Errorf("X-Forwarded-For = %q, want client address appended", xff)
	}
}

// TestProxyClientDisconnectDuringWait: when the client disappears while the
// proxy is waiting out a backoff, the handler aborts without writing a
// response and makes no further attempts.
func TestProxyClientDisconnectDuringWait(t *testing.T) {
	mt := newMockTransport(
		mockResult{resp: respFor(500, "boom", nil)},
		mockResult{resp: respFor(200, "never reached", nil)},
	)
	p := testProxy(mt, nil)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/http+status=500;*.initial=500ms;*.jitter=none/up.example.com/x", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(w, req) // long backoff wait
	}()

	// Wait for the first (failing) attempt, then disconnect the client.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mt.mu.Lock()
		calls := mt.calls
		mt.mu.Unlock()
		if calls >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if mt.calls != 1 {
		t.Errorf("transport calls = %d, want 1 (disconnect aborts the retry loop)", mt.calls)
	}
	// Nothing should have been written to the client: the recorder never saw
	// a WriteHeader call, so Code stays at its zero-value default (200) and
	// the body must be empty.
	if w.Body.Len() != 0 {
		t.Errorf("body written after client disconnect: %q", w.Body.String())
	}
}

// TestProxySchemeAndPortNormalization: explicit ports and scheme defaults
// land in the outbound URL authority correctly.
func TestProxySchemeAndPortNormalization(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/http/up.example.com/x", "http://up.example.com:80/x"},
		{"/https/up.example.com/x", "https://up.example.com:443/x"},
		{"/http/up.example.com:8080/x", "http://up.example.com:8080/x"},
		{"/https/up.example.com:8443/x", "https://up.example.com:8443/x"},
	}
	for _, tc := range cases {
		mt := newMockTransport(mockResult{resp: respFor(200, "ok", nil)})
		p := testProxy(mt, nil)
		w := do(p, "GET", tc.path, nil, nil)
		if w.Code != 200 {
			t.Errorf("%s: status = %d, want 200", tc.path, w.Code)
		}
		if got := mt.lastReq.URL.String(); got != tc.want {
			t.Errorf("%s: outbound URL = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestProxyBodyContentLengthPreserved: the outbound replay carries the
// captured body length so chunked-vs-fixed framing is upstream-visible.
func TestProxyBodyContentLengthPreserved(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "ok", nil)})
	p := testProxy(mt, nil)

	body := "0123456789"
	w := do(p, "POST", "/http/up.example.com/x", strings.NewReader(body), nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if mt.lastReq.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", mt.lastReq.ContentLength, len(body))
	}
	if mt.lastReq.Body == nil {
		t.Error("body not set on outbound request")
	}
}

// TestProxyPerStatusScopeShaping: only the scope matching the failing status
// shapes the wait — 429.initial=1ms does not soften 500's
// wait, and vice versa.
func TestProxyPerStatusScopeShaping(t *testing.T) {
	// 429 with a 1ms scoped wait: retries fast (well under the 300ms an
	// unscoped default would impose).
	mt := newMockTransport(
		mockResult{resp: respFor(429, "rate", nil)},
		mockResult{resp: respFor(200, "ok", nil)},
	)
	p := testProxy(mt, nil)

	start := time.Now()
	w := do(p, "GET", "/http+status=429,500;429.initial=1ms;429.max=2ms;*.initial=300ms;*.jitter=none/up.example.com/x", nil, nil)
	elapsed := time.Since(start)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("429 retry should use the retry[429] scope's 1ms wait; elapsed %s", elapsed)
	}
}

// TestProxyStatusScopeRaisesLimit: retry[500].attempts may exceed the
// default attempts; the loop honors the larger per-status limit.
func TestProxyStatusScopeRaisesLimit(t *testing.T) {
	mt := newMockTransport(
		mockResult{resp: respFor(500, "1", nil)},
		mockResult{resp: respFor(500, "2", nil)},
		mockResult{resp: respFor(200, "third time lucky", nil)},
	)
	p := testProxy(mt, nil)

	w := do(p, "GET", "/http+status=500;*.attempts=2;500.attempts=3;*.initial=1ms;*.jitter=none/up.example.com/x", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Retry-Limit"); got != "3" {
		t.Errorf("X-Retry-Limit = %q, want 3 (raised by retry[500].attempts)", got)
	}
	if mt.calls != 3 {
		t.Errorf("transport calls = %d, want 3", mt.calls)
	}
}

// TestProxyStatusGateClassShorthand: retry.status=5xx covers every code in
// the class.
func TestProxyStatusGateClassShorthand(t *testing.T) {
	for _, status := range []int{502, 503, 504} {
		mt := newMockTransport(
			mockResult{resp: respFor(status, "server noise", nil)},
			mockResult{resp: respFor(200, "ok", nil)},
		)
		p := testProxy(mt, nil)
		w := do(p, "GET", "/http+status=5xx;*.initial=1ms;*.jitter=none/up.example.com/x", nil, nil)
		if w.Code != 200 {
			t.Errorf("%d not retried under 5xx gate: status = %d", status, w.Code)
		}
		if mt.calls != 2 {
			t.Errorf("%d: transport calls = %d, want 2", status, mt.calls)
		}
	}
}

// TestProxyOnlyRetryParams: when there is no query at all, the
// upstream receives an empty query (no bare "?").
func TestProxyOnlyRetryParams(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "ok", nil)})
	p := testProxy(mt, nil)

	w := do(p, "GET", "/http+status=500;*.attempts=2/up.example.com/x", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := mt.lastReq.URL.RawQuery; got != "" {
		t.Errorf("upstream RawQuery = %q, want empty (policy in the segment, no query)", got)
	}
}

// TestProxySegmentPolicyQueryIsTargetData: the PRD's headline sentence — a
// segment policy drives retries while a retry.-shaped query key is target
// data that reaches the upstream verbatim (no split on ANY path).
func TestProxySegmentPolicyQueryIsTargetData(t *testing.T) {
	mt := newMockTransport(
		mockResult{resp: respFor(500, "first", nil)},
		mockResult{resp: respFor(200, "second", nil)},
	)
	p := testProxy(mt, nil)

	w := do(p, "GET", "/https+status=5xx;*.attempts=3/up.example.com/x?retry.count=5", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (policy drove a retry)", w.Code)
	}
	if got := mt.lastReq.URL.RawQuery; got != "retry.count=5" {
		t.Errorf("upstream RawQuery = %q, want retry.count=5 (target data, byte-identical)", got)
	}
	if got := w.Header().Get("X-Retry-Count"); got != "2" {
		t.Errorf("X-Retry-Count = %q, want 2 (lifecycle reported)", got)
	}
}

// TestProxyMethodPreserved: non-GET methods are relayed verbatim with their
// bodies replayed across retries.
func TestProxyMethodPreserved(t *testing.T) {
	for _, method := range []string{"PUT", "PATCH", "DELETE"} {
		mt := newMockTransport(mockResult{resp: respFor(200, "ok", nil)})
		p := testProxy(mt, nil)
		w := do(p, method, "/http/up.example.com/x", strings.NewReader("m="+method), nil)
		if w.Code != 200 {
			t.Errorf("%s: status = %d, want 200", method, w.Code)
		}
		if mt.lastReq.Method != method {
			t.Errorf("upstream method = %q, want %q", mt.lastReq.Method, method)
		}
	}
}

// TestProxyHeadRequest: HEAD responses relay without a body.
func TestProxyHeadRequest(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "", nil)})
	p := testProxy(mt, nil)

	w := do(p, "HEAD", "/http/up.example.com/x", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if mt.lastReq.Method != "HEAD" {
		t.Errorf("upstream method = %q, want HEAD", mt.lastReq.Method)
	}
}

// TestRoundTripTimeoutTable pins the per-try TTFB cap boundaries: a spent
// budget fails fast, a small remaining budget bounds the cap, and a healthy
// budget uses the default.
func TestRoundTripTimeoutTable(t *testing.T) {
	for _, tc := range []struct {
		remaining time.Duration
		want      time.Duration
	}{
		{0, time.Millisecond},
		{-1 * time.Second, time.Millisecond},
		{500 * time.Millisecond, 500 * time.Millisecond},
		{10 * time.Second, 10 * time.Second},
		{45 * time.Second, 30 * time.Second},
		{24 * time.Hour, 30 * time.Second},
	} {
		p := &Proxy{}
		if got := p.roundTripTimeout(tc.remaining); got != tc.want {
			t.Errorf("roundTripTimeout(%s) = %s, want %s", tc.remaining, got, tc.want)
		}
	}
}

// TestSleepCtx covers the backoff sleep: the full wait completes, and a
// canceled context aborts it early.
func TestSleepCtx(t *testing.T) {
	p := &Proxy{}

	start := time.Now()
	if !p.sleepCtx(context.Background(), 30*time.Millisecond) {
		t.Fatal("sleepCtx(30ms) = false, want true")
	}
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Errorf("sleepCtx returned after %s, want the full 30ms", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p.sleepCtx(ctx, time.Hour) {
		t.Error("sleepCtx with canceled context = true, want false")
	}
}

// TestProxyDegradedBodyForwarded pins the audit's degraded-mode semantics
// ("streaming pass-through"): an oversized, non-strict body must reach the
// upstream intact — the capture probe's consumed prefix spliced back in
// front of the unread remainder. Nothing is silently dropped.
func TestProxyDegradedBodyForwarded(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "ok", nil)})
	p := testProxy(mt, &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 8, DangerousAllowAll: true})

	w := do(p, "POST", "/http+status=500/up.example.com/x", strings.NewReader("0123456789"), nil)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	req := mt.lastReq
	if req == nil {
		t.Fatal("no outbound request captured")
	}
	if req.Body == nil {
		t.Fatal("outbound request has no body — degraded mode dropped it")
	}
	data, _ := io.ReadAll(req.Body)
	if string(data) != "0123456789" {
		t.Fatalf("upstream received body %q, want the full 10 bytes (streaming pass-through)", string(data))
	}
}

// TestProxyRacedResponseBodyClosed pins the resource-cleanup contract on the
// TTFB-timeout and client-disconnect paths: when the select in roundTrip picks
// the timer (or request-context cancellation) while the transport has just
// delivered a response, the response body must still be closed — otherwise the
// pooled connection never returns to the transport's idle pool (a leak per
// raced attempt).
func TestProxyRacedResponseBodyClosed(t *testing.T) {
	st := &racyTransport{}
	p := testProxy(st, nil)

	start := time.Now()
	w := do(p, "GET", "/http+budget=10ms/up.example.com/x", nil, nil)
	if w.Code != 504 {
		t.Fatalf("status = %d, want 504 (per-try TTFB bounded by the budget)", w.Code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("raced attempt should end at the budget cap; elapsed %s", elapsed)
	}
	// The raced response body must have been closed promptly after the timer
	// fired (the transport returns ~20ms after the 10ms cap).
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) && !st.closed.Load() {
		time.Sleep(2 * time.Millisecond)
	}
	if !st.closed.Load() {
		t.Error("LEAK: response body from the raced RoundTrip was never closed")
	}
}

// racyTransport returns a successful response slightly after the request's
// per-try TTFB cap, so the timer branch of the select races a ready result.
type racyTransport struct {
	closed atomic.Bool
}

func (s *racyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	time.Sleep(30 * time.Millisecond)
	b := newNotifyingBody()
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: b}
	go func() {
		<-b.closedCh
		s.closed.Store(true)
	}()
	return resp, nil
}

// notifyingBody reports its Close via a channel so tests can observe it.
type notifyingBody struct {
	Reader   *strings.Reader
	closedCh chan struct{}
}

func newNotifyingBody() *notifyingBody {
	return &notifyingBody{Reader: strings.NewReader("racy"), closedCh: make(chan struct{})}
}

func (n *notifyingBody) Read(p []byte) (int, error) { return n.Reader.Read(p) }
func (n *notifyingBody) Close() error {
	select {
	case <-n.closedCh:
	default:
		close(n.closedCh)
	}
	return nil
}

// ---- Batch 3: policy carriers, channels, and the policy-less pipeline ----

// testLogBuffer is a tiny io.Writer collecting log output.
type testLogBuffer struct {
	data []byte
}

func (b *testLogBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *testLogBuffer) String() string { return string(b.data) }

// captureLogs wires a Proxy's Log to a buffer so tests can assert on log
// lines (policy= on event=request).
func captureLogs(p *Proxy) *Proxy {
	buf := &testLogBuffer{}
	p.Log = log.New(buf, "", 0)
	return p
}

// logBuf returns the captured log output of a captureLogs-wired proxy.
func logBuf(p *Proxy) string {
	return p.Log.Writer().(*testLogBuffer).String()
}

// TestProxyPlainQueryVerbatimAndSingleCall: the policy-less plain form
// forwards RawQuery byte-identically — including retry.-prefixed TARGET data
// (the headline collision case) — and performs exactly one upstream call with
// no X-Retry-* response headers.
func TestProxyPlainQueryVerbatimAndSingleCall(t *testing.T) {
	// The raw query exercises every byte class the acceptance criterion
	// pins: %, +, valueless, duplicate, and retry.-prefixed target data.
	raw := "retry.count=7&a=%2Fb&c&d=&e=1&e=2&retry[429].token=x&f=%E4%B8%AD&g=p+q&retry.status=5xx"
	mt := newMockTransport(
		mockResult{resp: respFor(500, "upstream says no", nil)},
		mockResult{resp: respFor(200, "never retried", nil)},
	)
	p := testProxy(mt, nil)

	w := do(p, "GET", "/https/up.example.com/x?"+raw, nil, nil)
	if w.Code != 500 {
		t.Fatalf("status = %d, want 500 (the plain form relays the upstream verdict)", w.Code)
	}
	if mt.calls != 1 {
		t.Errorf("transport calls = %d, want 1 (no policy, no retry)", mt.calls)
	}
	if got := mt.lastReq.URL.RawQuery; got != raw {
		t.Errorf("upstream query = %q, want %q (byte-identical, retry.* is target data here)", got, raw)
	}
	for _, h := range []string{"X-Retry-Count", "X-Retry-Limit", "X-Retry-Exhausted", "X-Retry-Dropped"} {
		if got := w.Header().Get(h); got != "" {
			t.Errorf("%s = %q, want absent (no retry lifecycle without a policy)", h, got)
		}
	}
}

// TestProxyPlainNoRetryOnStatusGate: a retry.status spelling in the query
// is TARGET DATA on the policy-less plain form — it must not arm a status
// gate (single call on a 500 that would otherwise be gated).
func TestProxyPlainNoRetryOnStatusGate(t *testing.T) {
	mt := newMockTransport(
		mockResult{resp: respFor(500, "no", nil)},
		mockResult{resp: respFor(200, "never", nil)},
	)
	p := testProxy(mt, nil)

	w := do(p, "GET", "/http/up.example.com/x?retry.status=500&retry[*].attempts=3", nil, nil)
	if w.Code != 500 {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if mt.calls != 1 {
		t.Errorf("transport calls = %d, want 1 (query retry keys are not policy on the plain form)", mt.calls)
	}
}

// TestProxyPlainNetworkFailureNotRetried: the policy-less plain form has no
// network gate either — a dial failure exhausts on the first attempt (the
// observable difference from a network=1 policy).
func TestProxyPlainNetworkFailureNotRetried(t *testing.T) {
	mt := newMockTransport(
		mockResult{err: fmt.Errorf("dial tcp: connection refused")},
		mockResult{resp: respFor(200, "never retried", nil)},
	)
	p := testProxy(mt, nil)

	w := do(p, "GET", "/https/up.example.com/x", nil, nil)
	if w.Code != 504 {
		t.Fatalf("status = %d, want 504 (single attempt, network failure)", w.Code)
	}
	if mt.calls != 1 {
		t.Errorf("transport calls = %d, want 1", mt.calls)
	}
	for _, h := range []string{"X-Retry-Count", "X-Retry-Exhausted"} {
		if got := w.Header().Get(h); got != "" {
			t.Errorf("%s = %q, want absent without a policy", h, got)
		}
	}
}

// TestProxyPlainBodyStreamsNoCapture: the policy-less plain form captures
// nothing. A body far over the cap streams through with no 413, no
// X-Retry-Dropped, no degraded warn, byte-identical — even in strict mode.
func TestProxyPlainBodyStreamsNoCapture(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "ok", nil)})
	p := testProxy(mt, &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 8, StrictBodyLimit: true, DangerousAllowAll: true})
	body := strings.Repeat("0123456789", 100) // 1000 bytes >> 8-byte cap

	w := do(p, "POST", "/http/up.example.com/x", strings.NewReader(body), nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (no cap machinery without a policy, even strict)", w.Code)
	}
	if mt.calls != 1 {
		t.Errorf("transport calls = %d, want 1", mt.calls)
	}
	if got := w.Header().Get("X-Retry-Dropped"); got != "" {
		t.Errorf("X-Retry-Dropped = %q, want absent", got)
	}
	data, _ := io.ReadAll(mt.lastReq.Body)
	if string(data) != body {
		t.Errorf("upstream body len = %d, want %d (streamed intact)", len(data), len(body))
	}
	// Inbound framing is preserved on the policy-less streaming path (no
	// unconditional chunked re-framing — mutation-scan F-1): a fixed-length
	// inbound body must stay Content-Length framed upstream.
	if mt.lastReq.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d (inbound framing preserved)", mt.lastReq.ContentLength, len(body))
	}
}

// TestProxyPolicyChannelConflictMatrix: the two policy carriers are mutually
// exclusive — using both is a 400 naming each channel; each carrier alone
// works, and the policy-less plain form composes with the header (that IS
// the header channel, not a conflict).
func TestProxyPolicyChannelConflictMatrix(t *testing.T) {
	policyHdr := http.Header{}
	policyHdr.Set(RetryPolicyHeader, "status=5xx")

	tests := []struct {
		name      string
		path      string
		hdr       http.Header
		wantCode  int
		wantCalls int
	}{
		{"segment policy, no header", "/https+status=5xx/up.example.com/x", nil, 200, 1},
		{"segment policy + header", "/https+status=5xx/up.example.com/x", policyHdr, 400, 0},
		{"plain + header policy", "/https/up.example.com/x", policyHdr, 200, 1},
		{"plain, no policy anywhere", "/https/up.example.com/x", nil, 200, 1},
	}
	for _, tt := range tests {
		mt := newMockTransport(mockResult{resp: respFor(200, "ok", nil)})
		p := testProxy(mt, nil)
		w := do(p, "GET", tt.path, nil, tt.hdr)
		if w.Code != tt.wantCode {
			t.Errorf("%s: status = %d, want %d", tt.name, w.Code, tt.wantCode)
		}
		if mt.calls != tt.wantCalls {
			t.Errorf("%s: transport calls = %d, want %d", tt.name, mt.calls, tt.wantCalls)
		}
		if tt.wantCode == 400 {
			body := w.Body.String()
			for _, want := range []string{RetryPolicyHeader, "mutually exclusive", "scheme segment", "middleware or gateway"} {
				if !strings.Contains(body, want) {
					t.Errorf("%s: 400 body %q should name %q", tt.name, body, want)
				}
			}
		}
	}
}

// TestProxySegmentPolicyDegenerateHeader400: a degenerate policy header is
// still the header-layer 400 even when no policy rides the path (namespace
// checked before the conflict gate can misfire on an empty value).
func TestProxySegmentPolicyDegenerateHeader400(t *testing.T) {
	hdr := http.Header{}
	hdr.Set(RetryPolicyHeader, "")
	mt := newMockTransport(mockResult{resp: respFor(200, "no", nil)})
	p := testProxy(mt, nil)

	w := do(p, "GET", "/https+status=5xx/up.example.com/x", nil, hdr)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 (present-but-empty header is never absent)", w.Code)
	}
	if mt.calls != 0 {
		t.Errorf("transport calls = %d, want 0", mt.calls)
	}
}

// TestProxyPlainDegenerateHeader400: the plain form is where the
// header-layer degenerate 400 is the operative error (no conflict gate
// ahead of it). A present-but-empty policy header must 400, never fall
// through to the single-attempt literal.
func TestProxyPlainDegenerateHeader400(t *testing.T) {
	for _, val := range []string{"", "   ", "status=5xx;;network=1"} {
		hdr := http.Header{}
		hdr.Set(RetryPolicyHeader, val)
		mt := newMockTransport(mockResult{resp: respFor(200, "no", nil)})
		p := testProxy(mt, nil)

		w := do(p, "GET", "/https/up.example.com/x", nil, hdr)
		if w.Code != 400 {
			t.Fatalf("plain path + degenerate header %q: status = %d, want 400", val, w.Code)
		}
		if mt.calls != 0 {
			t.Errorf("degenerate header %q: transport calls = %d, want 0", val, mt.calls)
		}
	}
}

// TestProxySegmentPolicyUnknownReproxyHeader400: the reserved namespace
// applies on the segment-policy path too (unknown X-Reproxy-* is a 400
// in every configuration).
func TestProxySegmentPolicyUnknownReproxyHeader400(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("X-Reproxy-Bogus", "1")
	mt := newMockTransport(mockResult{resp: respFor(200, "no", nil)})
	p := testProxy(mt, nil)

	w := do(p, "GET", "/https+status=5xx/up.example.com/x", nil, hdr)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 (reserved namespace)", w.Code)
	}
	if mt.calls != 0 {
		t.Errorf("transport calls = %d, want 0", mt.calls)
	}
}

// TestProxyPlainUnknownReproxyHeader400: the plain form enforces the
// namespace too — ParseRetryPolicyHeader runs it internally.
func TestProxyPlainUnknownReproxyHeader400(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("X-Reproxy-Bogus", "1")
	mt := newMockTransport(mockResult{resp: respFor(200, "no", nil)})
	p := testProxy(mt, nil)

	w := do(p, "GET", "/https/up.example.com/x", nil, hdr)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 (reserved namespace without a policy)", w.Code)
	}
	if mt.calls != 0 {
		t.Errorf("transport calls = %d, want 0", mt.calls)
	}
}

// TestProxyPlainHeaderPolicyRetries: plain form + policy header = the
// intended collision-free combo — policy from the header, query untouched,
// retries observable, X-Retry-* emitted (a header policy IS a retry
// lifecycle).
func TestProxyPlainHeaderPolicyRetries(t *testing.T) {
	raw := "retry.count=9&retry.status=5xx"
	mt := newMockTransport(
		mockResult{resp: respFor(500, "boom", nil)},
		mockResult{resp: respFor(200, "ok", nil)},
	)
	p := testProxy(mt, nil)
	hdr := http.Header{}
	hdr.Set(RetryPolicyHeader, "status=5xx; [*].initial=1ms; [*].max=2ms; [*].jitter=none")

	w := do(p, "GET", "/https/up.example.com/x?"+raw, nil, hdr)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (header policy drove the retry)", w.Code)
	}
	if mt.calls != 2 {
		t.Errorf("transport calls = %d, want 2", mt.calls)
	}
	if got := mt.lastReq.URL.RawQuery; got != raw {
		t.Errorf("upstream query = %q, want %q (query is target data, untouched)", got, raw)
	}
	if got := w.Header().Get("X-Retry-Count"); got != "2" {
		t.Errorf("X-Retry-Count = %q, want 2 (header lifecycle reported)", got)
	}
}

// TestProxyPlainSingleAttemptLiteralNotParseDefaults: the policy-less plain
// form runs a SINGLE attempt even when the query carries status-gate-shaped
// keys; a 500 relays verbatim with no retry and no Parse(∅) default behavior
// (which would be 3 attempts).
func TestProxyPlainSingleAttemptLiteralNotParseDefaults(t *testing.T) {
	mt := newMockTransport(
		mockResult{resp: respFor(500, "first", nil)},
		mockResult{resp: respFor(500, "never", nil)},
		mockResult{resp: respFor(500, "never", nil)},
	)
	p := testProxy(mt, nil)

	w := do(p, "GET", "/https/up.example.com/x?retry.status=5xx", nil, nil)
	if w.Code != 500 {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if mt.calls != 1 {
		t.Errorf("transport calls = %d, want 1 (single-attempt LITERAL, not Parse defaults)", mt.calls)
	}
}

// TestProxyPlainHeaderPolicyCapturesBody: a header policy re-enables capture
// because the requested policy needs replay. A body over the cap degrades
// (X-Retry-Dropped), and strict mode 413s.
func TestProxyPlainHeaderPolicyCapturesBody(t *testing.T) {
	// Non-strict: oversized body with a header policy degrades to one shot.
	mt := newMockTransport(
		mockResult{resp: respFor(500, "no", nil)},
		mockResult{resp: respFor(500, "never", nil)},
	)
	p := testProxy(mt, &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 8, DangerousAllowAll: true})
	hdr := http.Header{}
	hdr.Set(RetryPolicyHeader, "status=5xx; [*].attempts=3")

	w := do(p, "POST", "/https/up.example.com/x", strings.NewReader("0123456789"), hdr)
	if w.Code != 500 {
		t.Fatalf("status = %d, want 500 (degraded single shot)", w.Code)
	}
	if got := w.Header().Get("X-Retry-Dropped"); got != "body-too-large" {
		t.Errorf("X-Retry-Dropped = %q, want body-too-large (capture revived by the header policy)", got)
	}
	if mt.calls != 1 {
		t.Errorf("transport calls = %d, want 1", mt.calls)
	}

	// Strict: oversized body with a header policy is 413 (replay impossible).
	mt2 := newMockTransport(mockResult{resp: respFor(200, "no", nil)})
	p2 := testProxy(mt2, &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 8, StrictBodyLimit: true, DangerousAllowAll: true})
	w2 := do(p2, "POST", "/https/up.example.com/x", strings.NewReader("0123456789"), hdr)
	if w2.Code != 413 {
		t.Fatalf("status = %d, want 413 (strict + capture revived)", w2.Code)
	}
}

// TestProxyPlainHeaderPolicyBodyReplayed: a captured body under a header
// policy replays verbatim across attempts.
func TestProxyPlainHeaderPolicyBodyReplayed(t *testing.T) {
	mt := newMockTransport(
		mockResult{resp: respFor(500, "boom", nil)},
		mockResult{resp: respFor(500, "boom", nil)},
		mockResult{resp: respFor(200, "ok", nil)},
	)
	rt := &bodyRecordingTransport{inner: mt}
	p := testProxy(rt, nil)
	hdr := http.Header{}
	hdr.Set(RetryPolicyHeader, "status=5xx; [*].initial=1ms; [*].jitter=none")

	w := do(p, "POST", "/https/up.example.com/x", strings.NewReader("payload-123"), hdr)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(rt.bodies) != 3 {
		t.Fatalf("attempts recorded = %d, want 3", len(rt.bodies))
	}
	for i, b := range rt.bodies {
		if b != "payload-123" {
			t.Errorf("attempt %d body = %q, want replay", i+1, b)
		}
	}
}

// TestProxyPlainSSRFStillEnforced: SSRF gates apply identically on the
// policy-less plain form — private resolution still 403.
func TestProxyPlainSSRFStillEnforced(t *testing.T) {
	mt := newMockTransport(mockResult{resp: respFor(200, "no", nil)})
	p := &Proxy{
		Config:    &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 10 << 20, DangerousAllowAll: true},
		Transport: mt,
		Resolver:  mockResolverBuilder("10.0.0.1"),
	}

	w := do(p, "GET", "/https/intranet.example.com/x", nil, nil)
	if w.Code != 403 {
		t.Fatalf("status = %d, want 403 (SSRF unconditional)", w.Code)
	}
	if mt.calls != 0 {
		t.Errorf("transport calls = %d, want 0", mt.calls)
	}
}

// TestProxyRequestLogCarriesPolicySource: event=request gains policy= so
// operators can see which carrier supplied the policy (or none).
func TestProxyRequestLogCarriesPolicySource(t *testing.T) {
	for _, tc := range []struct{ path, wantPolicy string }{
		{"/https+status=5xx/up.example.com/x", "segment"},
		{"/https/up.example.com/x", "header"},
		{"/https/up.example.com/x", "none"},
	} {
		mt := newMockTransport(mockResult{resp: respFor(200, "ok", nil)})
		p := captureLogs(testProxy(mt, nil))
		var hdr http.Header
		if tc.wantPolicy == "header" {
			hdr = http.Header{}
			hdr.Set(RetryPolicyHeader, "status=5xx")
		}
		do(p, "GET", tc.path, nil, hdr)
		out := logBuf(p)
		if !strings.Contains(out, "event=request") {
			t.Fatalf("%s: no event=request line in %q", tc.path, out)
		}
		if !strings.Contains(out, "policy="+tc.wantPolicy+" ") {
			t.Errorf("%s: log %q should carry policy=%s", tc.path, out, tc.wantPolicy)
		}
	}
}

// TestProxyPlainSchemeQueryNeverSplit: on the plain form the query is never
// inspected, so even an unknown retry.-shaped key is target data that
// reaches the upstream VERBATIM (500 relay, one call, no 400, no retry).
func TestProxyPlainSchemeQueryNeverSplit(t *testing.T) {
	raw := "retry.wat=1&data=%2Fpath"
	mt := newMockTransport(
		mockResult{resp: respFor(500, "upstream says no", nil)},
		mockResult{resp: respFor(200, "never retried", nil)},
	)
	p := testProxy(mt, nil)

	w := do(p, "GET", "/http/up.example.com/x?"+raw, nil, nil)
	if w.Code != 500 {
		t.Fatalf("status = %d, want 500 (plain form relays the upstream verdict)", w.Code)
	}
	if mt.calls != 1 {
		t.Errorf("transport calls = %d, want 1 (no retry lifecycle on the plain form)", mt.calls)
	}
	if got := mt.lastReq.URL.RawQuery; got != raw {
		t.Errorf("upstream query = %q, want %q (byte-identical, retry.* is target data)", got, raw)
	}
	for _, h := range []string{"X-Retry-Count", "X-Retry-Limit", "X-Retry-Exhausted", "X-Retry-Dropped"} {
		if got := w.Header().Get(h); got != "" {
			t.Errorf("%s = %q, want absent (no retry lifecycle on the plain form)", h, got)
		}
	}
}

// TestProxyPlainSchemeHeaderPolicyRetries: plain form + policy header = the
// intended header-channel combo — the policy drives retries, the query
// stays untouched, and X-Retry-* reports the lifecycle. NOT a conflict:
// only a policy in the scheme segment would conflict.
func TestProxyPlainSchemeHeaderPolicyRetries(t *testing.T) {
	raw := "retry.count=4&data=%2Fpath"
	mt := newMockTransport(
		mockResult{resp: respFor(500, "boom", nil)},
		mockResult{resp: respFor(200, "ok", nil)},
	)
	p := testProxy(mt, nil)
	hdr := http.Header{}
	hdr.Set(RetryPolicyHeader, "status=5xx; [*].initial=1ms; [*].max=2ms; [*].jitter=none")

	w := do(p, "GET", "/http/up.example.com/x?"+raw, nil, hdr)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (header policy drove the retry on the plain form)", w.Code)
	}
	if mt.calls != 2 {
		t.Errorf("transport calls = %d, want 2 (plain + header is a retry lifecycle)", mt.calls)
	}
	if got := mt.lastReq.URL.RawQuery; got != raw {
		t.Errorf("upstream query = %q, want %q (query is target data, untouched)", got, raw)
	}
	if got := w.Header().Get("X-Retry-Count"); got != "2" {
		t.Errorf("X-Retry-Count = %q, want 2 (header lifecycle reported)", got)
	}
}

// TestSingleAttemptPolicyLiteral: the D13 literal is one attempt, all gates
// off — and never the Parse(∅) v0.1.0 defaults.
func TestSingleAttemptPolicyLiteral(t *testing.T) {
	p := SingleAttemptPolicy(nil)
	if p.Default.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", p.Default.Attempts)
	}
	if p.NetworkGate {
		t.Error("NetworkGate = true, want false")
	}
	if len(p.StatusGate) != 0 {
		t.Errorf("StatusGate = %v, want empty", p.StatusGate)
	}
	// Discriminators against Parse(∅): that path returns 3 attempts,
	// network=1. A mutation flipping the literal to Parse(∅) fails here.
	q, err := Parse(url.Values{}, nil)
	if err != nil {
		t.Fatalf("Parse(empty) error: %v", err)
	}
	if q.Default.Attempts == p.Default.Attempts && q.NetworkGate == p.NetworkGate {
		t.Error("literal is indistinguishable from Parse(empty) defaults — D13 violated")
	}
	// Server clamp narrows the (TTFB-feeding) budget, never widens.
	cfg := &ServerConfig{MaxBudget: 100 * time.Millisecond}
	if b := SingleAttemptPolicy(cfg).Budget; b != 100*time.Millisecond {
		t.Errorf("Budget = %s, want clamped 100ms", b)
	}
}
