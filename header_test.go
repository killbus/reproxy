package reproxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// headerFor builds an http.Header from key/value pairs.
func headerFor(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

// TestHeaderPairGrammar: valid pair lists parse to the exact retry.-prefixed
// url.Values shape the policy carriers produce.
func TestHeaderPairGrammar(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  map[string]string
	}{
		{"full policy from design", "status=5xx; network=1; budget=30s; [*].attempts=3; [429].attempts=5", map[string]string{
			"retry.status":        "5xx",
			"retry.network":       "1",
			"retry.budget":        "30s",
			"retry[*].attempts":   "3",
			"retry[429].attempts": "5",
		}},
		{"single gate", "status=429", map[string]string{"retry.status": "429"}},
		{"scope only", "[429].attempts=2", map[string]string{"retry[429].attempts": "2"}},
		{"default scope only", "[*].initial=100ms", map[string]string{"retry[*].initial": "100ms"}},
		{"whitespace around pairs", "  status=429  ;  network=0  ", map[string]string{"retry.status": "429", "retry.network": "0"}},
		{"whitespace before separator is pair whitespace", "status=429 ; network=0", map[string]string{"retry.status": "429", "retry.network": "0"}},
		{"whitespace around the equals sign", "status = 429; network = 0", map[string]string{"retry.status": "429", "retry.network": "0"}},
		{"no whitespace at all", "status=429;network=0;budget=10s", map[string]string{"retry.status": "429", "retry.network": "0", "retry.budget": "10s"}},
		{"value with comma is fine", "status=429,500-599", map[string]string{"retry.status": "429,500-599"}},
		{"jitter and retry_after scope fields", "[*].jitter=equal; [429].retry_after=ignore", map[string]string{
			"retry[*].jitter":        "equal",
			"retry[429].retry_after": "ignore",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := headerFor(RetryPolicyHeader, tt.value)
			got, err := ParseRetryPolicyHeader(h)
			if err != nil {
				t.Fatalf("ParseRetryPolicyHeader(%q) error: %v", tt.value, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("params = %v, want %d keys", got, len(tt.want))
			}
			for key, wantVal := range tt.want {
				if vals := got[key]; len(vals) != 1 || vals[0] != wantVal {
					t.Errorf("params[%q] = %v, want [%q]", key, vals, wantVal)
				}
			}
		})
	}
}

// TestHeaderScopeKeysNeverDoubleDot: scope keys map to "retry[*].x", never
// "retry.[*].x" — the header carrier's prefixing rule (queryKeyForHeaderKey).
func TestHeaderScopeKeysNeverDoubleDot(t *testing.T) {
	got, err := ParseRetryPolicyHeader(headerFor(RetryPolicyHeader, "[*].attempts=2; [429].max=4s"))
	if err != nil {
		t.Fatalf("ParseRetryPolicyHeader error: %v", err)
	}
	for _, key := range []string{"retry[*].attempts", "retry[429].max"} {
		if _, ok := got[key]; !ok {
			t.Errorf("params missing %q (got %v)", key, got)
		}
	}
	if _, ok := got["retry.[*].attempts"]; ok {
		t.Errorf("params contains double-dot spelling \"retry.[*].attempts\": %v", got)
	}
}

// TestHeaderAbsentIsOnlyNoPolicySpelling: no header anywhere in the
// X-Reproxy-* namespace → (nil, nil), no 400.
func TestHeaderAbsentIsOnlyNoPolicySpelling(t *testing.T) {
	tests := []struct {
		name string
		h    http.Header
	}{
		{"nil header", nil},
		{"empty header", http.Header{}},
		{"unrelated headers only", headerFor("Accept", "application/json", "X-Custom", "data")},
		{"similar but outside namespace", headerFor("X-Reproxyx-Other", "v", "Xreproxy-Foo", "v")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRetryPolicyHeader(tt.h)
			if err != nil {
				t.Fatalf("ParseRetryPolicyHeader should not error, got: %v", err)
			}
			if got != nil {
				t.Errorf("params = %v, want nil (absent is no policy)", got)
			}
		})
	}
}

// TestHeaderDegenerateInputRejected: the normalization ladder — every
// degenerate form of a present header is a 400; only absence means "no
// policy" (design §2, hard requirement).
func TestHeaderDegenerateInputRejected(t *testing.T) {
	tests := []struct {
		name    string
		header  http.Header
		wantMsg string
	}{
		{
			"present but empty",
			headerFor(RetryPolicyHeader, ""),
			"is present but empty",
		},
		{
			"whitespace only",
			headerFor(RetryPolicyHeader, "   "),
			"is present but empty",
		},
		{
			"whitespace only with tabs",
			headerFor(RetryPolicyHeader, " \t "),
			"is present but empty",
		},
		{
			"empty pair in middle",
			headerFor(RetryPolicyHeader, "status=5xx;;network=1"),
			"empty pair",
		},
		{
			"trailing separator",
			headerFor(RetryPolicyHeader, "status=5xx;"),
			"empty pair",
		},
		{
			"leading separator",
			headerFor(RetryPolicyHeader, ";status=5xx"),
			"empty pair",
		},
		{
			"separator with whitespace pair",
			headerFor(RetryPolicyHeader, "status=5xx ; ; network=1"),
			"empty pair",
		},
		{
			"pair without equals",
			headerFor(RetryPolicyHeader, "status 5xx"),
			"not key=value",
		},
		{
			"pair without equals among valid",
			headerFor(RetryPolicyHeader, "status=5xx; network"),
			"not key=value",
		},
		{
			"empty key",
			headerFor(RetryPolicyHeader, "=5xx"),
			"empty key",
		},
		{
			"empty key among valid",
			headerFor(RetryPolicyHeader, "status=5xx; =1"),
			"empty key",
		},
		{
			"value containing equals",
			headerFor(RetryPolicyHeader, "status=429=500"),
			"contains \"=\"",
		},
		{
			"value containing equals among valid",
			headerFor(RetryPolicyHeader, "status=5xx; budget=1s=2s"),
			"contains \"=\"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRetryPolicyHeader(tt.header)
			if err == nil {
				t.Fatalf("ParseRetryPolicyHeader should reject %v, got params %v", tt.header, got)
			}
			if err.Code != 400 {
				t.Errorf("code = %d, want 400", err.Code)
			}
			if !strings.Contains(err.Reason, tt.wantMsg) {
				t.Errorf("reason = %q, want substring %q", err.Reason, tt.wantMsg)
			}
			if err.Hint == "" {
				t.Errorf("hint should not be empty for %q", tt.name)
			}
		})
	}
}

// TestHeaderMultipleOccurrencesRejected: more than one X-Reproxy-Retry-Policy
// header (any spelling) → 400 (fail closed).
func TestHeaderMultipleOccurrencesRejected(t *testing.T) {
	h := http.Header{}
	h.Add(RetryPolicyHeader, "status=5xx")
	h.Add(RetryPolicyHeader, "network=1")
	if got, err := ParseRetryPolicyHeader(h); err == nil {
		t.Fatalf("multiple headers should 400, got params %v", got)
	} else {
		if err.Code != 400 {
			t.Errorf("code = %d, want 400", err.Code)
		}
		if !strings.Contains(err.Reason, "multiple") {
			t.Errorf("reason = %q, want it to name the multiple occurrences", err.Reason)
		}
	}

	// Non-canonical spellings count as the same header (direct map
	// assignment keeps the key non-canonical; Add would canonicalize it).
	h2 := http.Header{}
	h2["x-reproxy-retry-policy"] = []string{"status=5xx"}
	h2["X-Reproxy-Retry-Policy"] = []string{"network=1"}
	if got, err := ParseRetryPolicyHeader(h2); err == nil {
		t.Fatalf("multiple (mixed-case) headers should 400, got params %v", got)
	}
}

// TestHeaderUnknownNamespaceRejected: any X-Reproxy-* header other than the
// policy header is a 400 naming it — the namespace is reserved in every mode.
func TestHeaderUnknownNamespaceRejected(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		bad    string
	}{
		{"unknown member alone", headerFor("X-Reproxy-Foo", "bar"), "X-Reproxy-Foo"},
		{"unknown member beside policy", headerFor(RetryPolicyHeader, "status=5xx", "X-Reproxy-Mode", "pure"), "X-Reproxy-Mode"},
		{"prefix-lookalike not claimed", headerFor("X-Reproxyx-Other", "v"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRetryPolicyHeader(tt.header)
			if tt.bad == "" {
				if err != nil {
					t.Fatalf("outside-namespace header must not 400, got: %v", err)
				}
				if got != nil {
					t.Errorf("params = %v, want nil (no policy header present)", got)
				}
				return
			}
			if err == nil {
				t.Fatalf("unknown namespace member %q should 400", tt.bad)
			}
			if err.Code != 400 {
				t.Errorf("code = %d, want 400", err.Code)
			}
			if !strings.Contains(err.Reason, tt.bad) {
				t.Errorf("reason = %q, want it to name %q", err.Reason, tt.bad)
			}
			if !strings.Contains(err.Hint, RetryPolicyHeader) {
				t.Errorf("hint = %q, want it to name the recognized header", err.Hint)
			}
		})
	}
}

// TestHeaderLegitimateValuesRepresentable: every legitimate value in the
// full field grammar contains neither ";" nor "=" — the pair split is
// unambiguous by construction, so the grammar needs no escaping.
func TestHeaderLegitimateValuesRepresentable(t *testing.T) {
	legit := []string{
		"429", "5xx", "4XX", "500-599", "429,500-599", "429,5xx",
		"0", "1",
		"30s", "90s", "1500ms",
		"3", "1", "10",
		"constant", "linear", "exponential",
		"none", "full", "equal",
		"honor", "ignore",
	}
	for _, v := range legit {
		if strings.ContainsAny(v, ";=") {
			t.Errorf("legitimate value %q contains a pair separator — grammar is ambiguous", v)
		}
		// And each round-trips through a minimal header.
		got, err := ParseRetryPolicyHeader(headerFor(RetryPolicyHeader, "status="+v))
		if err != nil {
			t.Fatalf("status=%q through header: %v", v, err)
		}
		if got.Get("retry.status") != v {
			t.Errorf("retry.status = %q, want %q", got.Get("retry.status"), v)
		}
	}
}

// TestHeaderTransformEquivalence: the same policy via the scheme segment
// (ParsePath) and via the header (ParseRetryPolicyHeader) resolves to
// deep-equal Policies — one grammar, two carriers, a pure channel swap.
func TestHeaderTransformEquivalence(t *testing.T) {
	segmentPath := "/https+status=429,5xx;network=1;budget=30s;*.attempts=3;*.backoff=linear;429.attempts=5/h"
	headerVal := "status=429,5xx; network=1; budget=30s; [*].attempts=3; [*].backoff=linear; [429].attempts=5"

	_, viaSegment, rerr := ParsePath(segmentPath)
	if rerr != nil {
		t.Fatalf("ParsePath error: %v", rerr)
	}
	viaHeader, herr := ParseRetryPolicyHeader(headerFor(RetryPolicyHeader, headerVal))
	if herr != nil {
		t.Fatalf("ParseRetryPolicyHeader error: %v", herr)
	}

	cfg := NewDefaultConfig()
	pseg, err1 := Parse(viaSegment, cfg)
	ph, err2 := Parse(viaHeader, cfg)
	if err1 != nil || err2 != nil {
		t.Fatalf("Parse errors: segment=%v header=%v", err1, err2)
	}
	if !reflect.DeepEqual(pseg, ph) {
		t.Errorf("segment-channel Policy %+v != header-channel Policy %+v", pseg, ph)
	}

	// Also identical as url.Values maps (same key spellings, same values).
	if !reflect.DeepEqual(viaSegment, viaHeader) {
		t.Errorf("url.Values differ: segment=%v header=%v", viaSegment, viaHeader)
	}
}

// TestHeaderParseErrorMatrix: policy.Parse's own 400 matrix — unknown keys,
// bad values, dead config, cross-field violations — fires identically
// through the header path, with Parse's error bodies.
func TestHeaderParseErrorMatrix(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantMsg string
	}{
		{"unknown gate", "attempt=3", "unknown retry parameter"},
		{"unknown scope field", "[*].bogus=1", "unknown retry parameter"},
		{"gate inside scope", "[429].status=5xx", "unknown retry parameter"},
		{"bad attempts", "[*].attempts=abc", "invalid value"},
		{"attempts zero", "[*].attempts=0", "invalid value"},
		{"bad backoff", "[*].backoff=fast", "invalid value"},
		{"bad duration", "[*].initial=100", "invalid value"},
		{"bad budget", "budget=10", "invalid value"},
		{"bad network value", "network=yes", "invalid value"},
		{"bad status list", "status=abc", "invalid status code"},
		{"empty status item", "status=429,,500", "empty item"},
		{"dead config", "status=429; [500].attempts=2", "dead retry configuration"},
		{"max below initial", "[*].initial=4s; [*].max=1s", "max (1s) is smaller than initial (4s)"},
		// A non-3-digit scope is NOT splitScopeKey's exact-status shape, so
		// Parse's default branch reports it as an unknown parameter — that
		// IS Parse's verdict for this spelling (identical to the query
		// channel: "?retry[42].attempts=2" yields the same 400 body).
		{"bad scope digits", "[42].attempts=2", "unknown retry parameter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			viaHeader, rerr := ParseRetryPolicyHeader(headerFor(RetryPolicyHeader, tt.value))
			if rerr != nil {
				t.Fatalf("header-level parse error (pair grammar should accept %q): %v", tt.value, rerr)
			}
			_, perr := Parse(viaHeader, NewDefaultConfig())
			if perr == nil {
				t.Fatalf("Parse should reject %q through the header channel", tt.value)
			}
			if perr.Code != 400 {
				t.Errorf("code = %d, want 400", perr.Code)
			}
			if !strings.Contains(perr.Reason, tt.wantMsg) {
				t.Errorf("reason = %q, want substring %q", perr.Reason, tt.wantMsg)
			}
		})
	}
}

// TestHeaderParseRoundTrip: a full valid policy through the header channel
// resolves to the same Policy the equivalent query produces (spot-check on
// resolved fields, complementing the deep-equal test above with different
// input).
func TestHeaderParseRoundTrip(t *testing.T) {
	viaHeader, rerr := ParseRetryPolicyHeader(headerFor(RetryPolicyHeader, "status=429; [*].attempts=4; [429].attempts=2"))
	if rerr != nil {
		t.Fatalf("ParseRetryPolicyHeader error: %v", rerr)
	}
	p, perr := Parse(viaHeader, NewDefaultConfig())
	if perr != nil {
		t.Fatalf("Parse error: %v", perr)
	}
	if p.Default.Attempts != 4 {
		t.Errorf("Default.Attempts = %d, want 4", p.Default.Attempts)
	}
	if sp := p.Effective(429); sp.Attempts != 2 {
		t.Errorf("Effective(429).Attempts = %d, want 2", sp.Attempts)
	}
	if !p.RetryableStatus(429) {
		t.Error("429 should be retryable")
	}
	if p.RetryableStatus(500) {
		t.Error("500 should not be retryable")
	}
}

// TestHeaderParseEmptyVsAbsent: the transform must preserve the
// absent-vs-empty distinction's consequences — absent header yields nil
// params (caller decides, e.g. the pure-mode single-attempt literal), and a
// degenerate present header never reaches Parse at all. Also documents
// Parse's behavior for the caller: Parse(nil-or-empty) returns the v0.1.0
// defaults (3 attempts, network=1) — never a single attempt.
func TestHeaderParseEmptyVsAbsent(t *testing.T) {
	// Absent: nil params, no error.
	got, err := ParseRetryPolicyHeader(http.Header{})
	if err != nil || got != nil {
		t.Fatalf("absent header: got (%v, %v), want (nil, nil)", got, err)
	}

	// Degenerate present: 400 at the header layer (never reaches Parse).
	if _, err := ParseRetryPolicyHeader(headerFor(RetryPolicyHeader, "")); err == nil {
		t.Fatal("empty present header must 400")
	}

	// Parse's documented behavior for the empty transform: v0.1.0 defaults,
	// NOT single-attempt. Batch 3 must not rely on Parse(nil) meaning "off".
	p, perr := Parse(url.Values{}, NewDefaultConfig())
	if perr != nil {
		t.Fatalf("Parse(empty) error: %v", perr)
	}
	if p.Default.Attempts != DefaultAttempts || !p.NetworkGate {
		t.Errorf("Parse(empty) = %+v, want v0.1.0 defaults (attempts=%d, network on)", p, DefaultAttempts)
	}
}

// TestHeaderNonCanonicalSpellingAccepted: http.Header from the wire is
// canonical, but hand-built maps may not be — the parse must be spelling-proof.
// (Direct map assignment keeps the key non-canonical; Set/Add would fix it.)
func TestHeaderNonCanonicalSpellingAccepted(t *testing.T) {
	h := http.Header{}
	h["x-reproxy-retry-policy"] = []string{"status=5xx"}
	got, err := ParseRetryPolicyHeader(h)
	if err != nil {
		t.Fatalf("non-canonical spelling should parse: %v", err)
	}
	if got.Get("retry.status") != "5xx" {
		t.Errorf("retry.status = %q, want 5xx", got.Get("retry.status"))
	}
}

// TestBuildOutboundHeadersStripsReproxyNamespace: X-Reproxy-* headers never
// reach the upstream (they are reproxy<->client protocol, like hop-by-hop
// headers); other client headers still pass through.
func TestBuildOutboundHeadersStripsReproxyNamespace(t *testing.T) {
	inbound := http.Header{}
	inbound.Set(RetryPolicyHeader, "status=5xx")
	inbound.Set("X-Reproxy-Some-Future-Thing", "v")
	inbound.Set("x-reproxy-lowercase", "v")
	inbound.Set("X-Keep", "client data")
	r := httptestRequest("GET", "/http/up.example.com/x")

	out := buildOutboundHeaders(inbound, r)
	for name := range out {
		if isReproxyHeader(name) {
			t.Errorf("X-Reproxy-* header %q forwarded upstream", name)
		}
	}
	if out.Get("X-Keep") != "client data" {
		t.Errorf("X-Keep not preserved: %v", out["X-Keep"])
	}
}

// TestBuildOutboundHeadersStripsReproxyNamespaceNonCanonical: non-canonical
// spellings in a hand-built header map are stripped too (direct map
// assignment keeps the key non-canonical; Set would canonicalize it).
func TestBuildOutboundHeadersStripsReproxyNamespaceNonCanonical(t *testing.T) {
	inbound := http.Header{}
	inbound["x-reproxy-retry-policy"] = []string{"status=5xx"}
	r := httptestRequest("GET", "/http/up.example.com/x")

	out := buildOutboundHeaders(inbound, r)
	for name := range out {
		if isReproxyHeader(name) {
			t.Errorf("X-Reproxy-* (non-canonical) %q survived the strip", name)
		}
	}
}

// TestIsReproxyHeader: the namespace check itself.
func TestIsReproxyHeader(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"X-Reproxy-Retry-Policy", true},
		{"x-reproxy-retry-policy", true},
		{"X-Reproxy-Foo", true},
		{"X-reproxy-anything", true},
		{"X-Reproxyx-Other", false},
		{"Xreproxy-Foo", false},
		{"X-Custom", false},
		{"Retry-After", false},
	}
	for _, tt := range tests {
		if got := isReproxyHeader(tt.name); got != tt.want {
			t.Errorf("isReproxyHeader(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestValidateReproxyNamespaceDirect: the standalone namespace guard Batch 3
// calls on the query-channel path (where the policy header is a conflict,
// not a parse target).
func TestValidateReproxyNamespaceDirect(t *testing.T) {
	if err := validateReproxyNamespace(headerFor("Accept", "application/json")); err != nil {
		t.Errorf("clean header set should pass, got: %v", err)
	}
	if err := validateReproxyNamespace(headerFor(RetryPolicyHeader, "status=5xx")); err != nil {
		t.Errorf("the policy header itself is a known member, got: %v", err)
	}
	err := validateReproxyNamespace(headerFor("X-Reproxy-Bogus", "1"))
	if err == nil || err.Code != 400 {
		t.Fatalf("unknown member should 400, got %v", err)
	}
	if !strings.Contains(err.Reason, "X-Reproxy-Bogus") {
		t.Errorf("reason = %q, want it to name the header", err.Reason)
	}
}

// httptestRequest is a minimal request builder for buildOutboundHeaders tests.
func httptestRequest(method, target string) *http.Request {
	return httptest.NewRequest(method, target, nil)
}
