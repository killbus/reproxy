package reproxy

// e2e_test.go drives the full stack end to end: a real proxy listener
// (httptest.NewServer around the Proxy handler), a real upstream
// (httptest.NewServer), and a real http.Client with redirect-following
// disabled. No mock transports — every scenario crosses real TCP.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// e2eEnv bundles one proxy listener with one upstream and the client used
// to drive it. The upstream's hostname is "up.example.com" from the proxy's
// perspective (the pinned-resolver mock maps it to a public IP while the dial
// redirects to the upstream's real listener), so allowlist matching, target
// parsing, and retry logic all run at full fidelity.
type e2eEnv struct {
	proxy    *httptest.Server
	upstream *httptest.Server
	client   *http.Client
	// upstreamHost is the upstream hostname as targets address it.
	upstreamHost string
	// calls counts requests that reached the upstream.
	calls *int32
}

// newE2EEnv starts a real upstream under the given handler, wires a Proxy
// whose resolver validates a public IP (SSRF L3 sees a public pin) and whose
// dialer redirects to the upstream listener, and serves the proxy on a real
// listener. The upstream is addressed as up.example.com in proxy targets.
func newE2EEnv(t *testing.T, upstreamHandler http.HandlerFunc, cfg *ServerConfig) *e2eEnv {
	t.Helper()

	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstream.Close)

	uhost, uport, err := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		t.Fatalf("split upstream addr: %v", err)
	}

	if cfg == nil {
		cfg = &ServerConfig{
			MaxAttempts:       10,
			MaxBudget:         10 * time.Second,
			MaxBody:           10 << 20,
			DangerousAllowAll: true,
		}
	}

	pr := &PinnedResolver{
		Lookup: mockLookupBuilder("93.184.216.10"),
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(uhost, uport))
		},
	}
	pr.Transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           pr.DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	proxy := &Proxy{
		Config:    cfg,
		Transport: pr.Transport,
		Resolver:  pr,
	}
	psrv := httptest.NewServer(proxy)
	t.Cleanup(psrv.Close)

	return &e2eEnv{
		proxy:        psrv,
		upstream:     upstream,
		upstreamHost: "up.example.com",
		calls:        new(int32),
		client: &http.Client{
			Transport: &http.Transport{},
			// reproxy relays 3xx verbatim; the client must not follow either.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Timeout: 15 * time.Second,
		},
	}
}

// get performs a client GET against the proxy with the given target path
// (e.g. "/http/up.example.com/x?retry.status=500").
func (e *e2eEnv) get(t *testing.T, target string) *http.Response {
	t.Helper()
	resp, err := e.client.Get(e.proxy.URL + target)
	if err != nil {
		t.Fatalf("client GET %s: %v", target, err)
	}
	return resp
}

// doReq performs an arbitrary method request with a body.
func (e *e2eEnv) doReq(t *testing.T, method, target string, body io.Reader, hdr http.Header) *http.Response {
	t.Helper()
	var req *http.Request
	var err error
	if hdr != nil {
		req, err = http.NewRequest(method, e.proxy.URL+target, body)
	} else {
		req, err = http.NewRequest(method, e.proxy.URL+target, body)
	}
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if hdr != nil {
		req.Header = hdr
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("client %s %s: %v", method, target, err)
	}
	return resp
}

// bodyString reads and closes the response body.
func bodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return string(b)
}

// countedUpstream wraps a handler that counts requests via the env's counter.
func (e *e2eEnv) counted(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(e.calls, 1)
		h(w, r)
	}
}

// TestE2EHappyPathGET: /http/<host>/path?x=1 relays the upstream response
// with X-Retry-Count: 1 and the original query intact.
func TestE2EHappyPathGET(t *testing.T) {
	env := newE2EEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "x=1" {
			t.Errorf("upstream query = %q, want x=1", r.URL.RawQuery)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("hello from upstream"))
	}, nil)
	_ = env

	resp := env.get(t, "/http/up.example.com/path?x=1")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Retry-Count"); got != "1" {
		t.Errorf("X-Retry-Count = %q, want 1", got)
	}
	if got := bodyString(t, resp); got != "hello from upstream" {
		t.Errorf("body = %q", got)
	}
}

// TestE2ERetryToSuccess: two 500s then a 200 with a status gate and 3
// attempts -> the client sees the 200 after exactly 3 upstream hits.
func TestE2ERetryToSuccess(t *testing.T) {
	var n int32
	env := newE2EEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) <= 2 {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("boom"))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("third time"))
	}, nil)

	resp := env.get(t, "/http/up.example.com/x?retry.status=500&retry[*].attempts=3&retry[*].initial=1ms&retry[*].max=2ms&retry[*].jitter=none")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := bodyString(t, resp); got != "third time" {
		t.Errorf("body = %q", got)
	}
	if got := resp.Header.Get("X-Retry-Count"); got != "3" {
		t.Errorf("X-Retry-Count = %q, want 3", got)
	}
	if got := resp.Header.Get("X-Retry-Limit"); got != "3" {
		t.Errorf("X-Retry-Limit = %q, want 3", got)
	}
	if n := atomic.LoadInt32(&n); n != 3 {
		t.Errorf("upstream calls = %d, want 3", n)
	}
}

// TestE2EExhaustionDeliversLastResponse: an always-500 upstream with
// attempts=2 -> the client receives the real 500 (the upstream verdict),
// flagged with X-Retry-Exhausted and X-Retry-Count: 2.
func TestE2EExhaustionDeliversLastResponse(t *testing.T) {
	var n int32
	env := newE2EEnv(t, func(w http.ResponseWriter, r *http.Request) {
		k := atomic.AddInt32(&n, 1)
		w.WriteHeader(500)
		_, _ = w.Write([]byte(fmt.Sprintf("attempt-%d", k)))
	}, nil)

	resp := env.get(t, "/http/up.example.com/x?retry.status=500&retry[*].attempts=2&retry[*].initial=1ms&retry[*].max=2ms&retry[*].jitter=none")
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500 (real upstream verdict)", resp.StatusCode)
	}
	if got := bodyString(t, resp); got != "attempt-2" {
		t.Errorf("body = %q, want the LAST attempt's body", got)
	}
	if got := resp.Header.Get("X-Retry-Exhausted"); got != "1" {
		t.Errorf("X-Retry-Exhausted = %q, want 1", got)
	}
	if got := resp.Header.Get("X-Retry-Count"); got != "2" {
		t.Errorf("X-Retry-Count = %q, want 2", got)
	}
	if n := atomic.LoadInt32(&n); n != 2 {
		t.Errorf("upstream calls = %d, want 2", n)
	}
}

// TestE2EPOSTBodyReplay: the upstream echoes the body it received; a POST
// that fails once then succeeds must deliver the echoed body on the second
// attempt — pinning replay integrity across a real network round trip.
func TestE2EPOSTBodyReplay(t *testing.T) {
	var n int32
	env := newE2EEnv(t, func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream read body: %v", err)
			w.WriteHeader(500)
			return
		}
		k := atomic.AddInt32(&n, 1)
		if k == 1 {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("first try fails"))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("echo:" + string(b)))
	}, nil)

	resp := env.doReq(t, "POST", "/http/up.example.com/echo?retry.status=500&retry[*].attempts=2&retry[*].initial=1ms&retry[*].max=2ms&retry[*].jitter=none",
		strings.NewReader("hello world"), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := bodyString(t, resp); got != "echo:hello world" {
		t.Errorf("body = %q, want echoed replay", got)
	}
	if n := atomic.LoadInt32(&n); n != 2 {
		t.Errorf("upstream calls = %d, want 2", n)
	}
}

// TestE2EPOSTOversizedDegraded: a body over --max-body in non-strict mode is
// forwarded once (no retry) with X-Retry-Dropped, and the upstream receives
// the FULL body byte-identically — pinning the streaming pass-through.
func TestE2EPOSTOversizedDegraded(t *testing.T) {
	var bodies []string
	var mu sync.Mutex
	var n int32
	cfg := &ServerConfig{
		MaxAttempts:       10,
		MaxBudget:         10 * time.Second,
		MaxBody:           16,
		DangerousAllowAll: true,
	}
	env := newE2EEnv(t, func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream read body: %v", err)
			w.WriteHeader(500)
			return
		}
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		atomic.AddInt32(&n, 1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	}, cfg)

	body := "0123456789abcdefghij" // 20 bytes > 16-byte cap
	resp := env.doReq(t, "POST", "/http/up.example.com/upload?retry.status=500&retry[*].attempts=3",
		strings.NewReader(body), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (degraded single pass-through)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Retry-Dropped"); got != "body-too-large" {
		t.Errorf("X-Retry-Dropped = %q, want body-too-large", got)
	}
	if got := resp.Header.Get("X-Retry-Count"); got != "1" {
		t.Errorf("X-Retry-Count = %q, want 1 (no retry in degraded mode)", got)
	}
	if n := atomic.LoadInt32(&n); n != 1 {
		t.Errorf("upstream calls = %d, want 1", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 || bodies[0] != body {
		t.Errorf("upstream bodies = %v, want the full 20-byte body byte-identical", bodies)
	}
}

// TestE2EQueryBytePreservation: the passthrough query reaches the upstream
// byte-identically — encoded slashes, plus signs, empty values, valueless
// keys, duplicates, and UTF-8 all round-trip exactly.
func TestE2EQueryBytePreservation(t *testing.T) {
	raw := "%2F=%2F&b=%20&c&d=&e=1&e=2"
	var got string
	var ok bool
	env := newE2EEnv(t, func(w http.ResponseWriter, r *http.Request) {
		got, ok = r.URL.RawQuery, true
		w.WriteHeader(200)
	}, nil)

	resp := env.get(t, "/http/up.example.com/p?"+raw+"&retry.status=500&retry[*].attempts=2")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	bodyString(t, resp)
	if !ok {
		t.Fatal("upstream never received the request")
	}
	if got != raw {
		t.Errorf("upstream RawQuery = %q, want %q (byte-identical)", got, raw)
	}
}

// TestE2ESSEStreaming: three SSE events with 100ms gaps must reach the
// client as they arrive, not buffered until the stream ends. The read
// timestamps are asserted, not just the final content.
func TestE2ESSEStreaming(t *testing.T) {
	env := newE2EEnv(t, func(w http.ResponseWriter, r *http.Request) {
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
			time.Sleep(100 * time.Millisecond)
		}
	}, nil)

	resp := env.get(t, "/http/up.example.com/stream")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	defer resp.Body.Close()

	// Timestamp only the "data:" lines (SSE frames are "data: x\n\n"; the
	// blank separator lines carry no timing signal).
	var reads []time.Time
	var events []string
	sc := bufio.NewScanner(resp.Body)
	start := time.Now()
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		reads = append(reads, time.Now())
		events = append(events, line)
		if len(events) == 3 {
			break
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan stream: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("events read = %d (%v), want 3", len(events), events)
	}
	for i, want := range []string{"data: one", "data: two", "data: three"} {
		if events[i] != want {
			t.Errorf("event[%d] = %q, want %q", i, events[i], want)
		}
	}
	// Streaming assertion: the first event must arrive promptly and the last
	// must arrive ~200ms later (3 events, 100ms apart). A buffered-until-end
	// response would deliver all three within a couple of milliseconds.
	first := reads[0].Sub(start)
	spread := reads[2].Sub(reads[0])
	if first > 60*time.Millisecond {
		t.Errorf("first event arrived after %s, want promptly (streamed)", first)
	}
	if spread < 150*time.Millisecond {
		t.Errorf("event spread = %s, want >= 150ms (events must arrive as they are sent)", spread)
	}
}

// TestE2ESSRFPrivateIPLiteral: a loopback IP-literal target is 403.
func TestE2ESSRFPrivateIPLiteral(t *testing.T) {
	env := newE2EEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached for a forbidden target")
		w.WriteHeader(200)
	}, nil)

	resp := env.get(t, "/http/127.0.0.1/x")
	if resp.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	bodyString(t, resp)
}

// TestE2ESSRFPrivateResolution: a hostname resolving to a private IP is a
// 403 (L3 fail-closed; nothing dialed).
func TestE2ESSRFPrivateResolution(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached for a private resolution")
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	pr := &PinnedResolver{Lookup: mockLookupBuilder("10.0.0.1")}
	proxy := &Proxy{
		Config:   &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 10 << 20, DangerousAllowAll: true},
		Resolver: pr,
	}
	psrv := httptest.NewServer(proxy)
	defer psrv.Close()

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(psrv.URL + "/http/intranet.example.com/x")
	if err != nil {
		t.Fatalf("client GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(mustBody(t, resp), "forbidden") {
		t.Errorf("403 body should name the forbidden resolution")
	}
}

// mustBody reads a body that must not be empty.
func mustBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// TestE2EAllowlistGateBeforeDNS: with the upstream host missing from the
// allowlist, the request is 403 and DNS is never consulted (L2 before L3).
func TestE2EAllowlistGateBeforeDNS(t *testing.T) {
	lookups := new(int32)
	pr := &PinnedResolver{
		Lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			atomic.AddInt32(lookups, 1)
			return mockLookupBuilder("93.184.216.10")(ctx, host)
		},
	}
	proxy := &Proxy{
		Config:   &ServerConfig{MaxAttempts: 10, MaxBudget: 5 * time.Second, MaxBody: 10 << 20, Allowlist: []string{"other.example.com"}},
		Resolver: pr,
	}
	psrv := httptest.NewServer(proxy)
	defer psrv.Close()

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(psrv.URL + "/http/up.example.com/x")
	if err != nil {
		t.Fatalf("client GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if n := atomic.LoadInt32(lookups); n != 0 {
		t.Errorf("DNS lookups = %d, want 0 (allowlist gate precedes DNS)", n)
	}
}

// TestE2EUnknownRetryKey400: an unknown retry.* key is a 400 naming the key
// in the JSON error body.
func TestE2EUnknownRetryKey400(t *testing.T) {
	env := newE2EEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached for a 400")
		w.WriteHeader(200)
	}, nil)

	resp := env.get(t, "/http/up.example.com/x?retry.wat=1")
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var e struct {
		Error string `json:"error"`
		Hint  string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(mustBody(t, resp)), &e); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if !strings.Contains(e.Error, "retry.wat") {
		t.Errorf("error = %q, want it to name the key", e.Error)
	}
}

// TestE2EDeadConfig400: retry[429].attempts without 429 in retry.status is
// a dead configuration -> 400 (fail closed).
func TestE2EDeadConfig400(t *testing.T) {
	env := newE2EEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached for a 400")
		w.WriteHeader(200)
	}, nil)

	resp := env.get(t, "/http/up.example.com/x?retry.status=500&retry[429].attempts=4")
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var e struct {
		Error string `json:"error"`
		Hint  string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(mustBody(t, resp)), &e); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if !strings.Contains(e.Error, "retry[429]") {
		t.Errorf("error = %q, want it to name the dead scope", e.Error)
	}
}

// TestE2EBudgetExhaustion: a tiny budget with a backoff initial larger than
// the budget -> the first retry's wait cannot fit, so the loop stops after
// the first attempt, the client gets the last (500) response with
// X-Retry-Exhausted, and the whole lifecycle stays bounded.
func TestE2EBudgetExhaustion(t *testing.T) {
	var n int32
	env := newE2EEnv(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(500)
		_, _ = w.Write([]byte("always down"))
	}, nil)

	start := time.Now()
	resp := env.get(t, "/http/up.example.com/x?retry.status=500&retry.budget=100ms&retry[*].attempts=10&retry[*].initial=5s&retry[*].max=10s&retry[*].jitter=none")
	elapsed := time.Since(start)
	defer bodyString(t, resp)

	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500 (last response delivered)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Retry-Exhausted"); got != "1" {
		t.Errorf("X-Retry-Exhausted = %q, want 1", got)
	}
	if elapsed > 2*time.Second {
		t.Errorf("budget should bound the lifecycle; elapsed %s", elapsed)
	}
	if n := atomic.LoadInt32(&n); n != 1 {
		t.Errorf("upstream calls = %d, want 1 (budget cut the loop short before any wait)", n)
	}
}

// TestE2EHostHeaderIsUpstreams: the outbound request's Host (and Host header)
// is the upstream's authority, not the proxy's.
func TestE2EHostHeaderIsUpstreams(t *testing.T) {
	var gotHost string
	env := newE2EEnv(t, func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.WriteHeader(200)
	}, nil)

	// The upstream sees the proxy's rewritten authority: up.example.com:80.
	resp := env.get(t, "/http/up.example.com/x")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	bodyString(t, resp)
	want := "up.example.com:80"
	if gotHost != want {
		t.Errorf("upstream saw Host %q, want %q (the upstream's own authority)", gotHost, want)
	}
}
