package reproxy

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsePath(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		want       PathTarget
		wantPolicy map[string]string // nil = no policy expected
		wantErr    string            // substring of Reason; empty means success expected
		wantHnt    string            // substring of Hint, checked when wantErr != ""
	}{
		// --- Missing / malformed target shape ---
		{"empty path", "", PathTarget{}, nil, "missing upstream target", "SCHEME"},
		{"bare slash", "/", PathTarget{}, nil, "missing upstream target", "SCHEME"},
		{"scheme only", "/https", PathTarget{}, nil, "missing upstream authority", "SCHEME"},
		{"scheme and slash", "/https/", PathTarget{}, nil, "missing upstream authority", "SCHEME"},
		{"double slash after host slash", "//example.com", PathTarget{}, nil, "missing upstream target", "SCHEME"},
		{"unknown scheme ftp", "/ftp/example.com/x", PathTarget{}, nil, "unsupported scheme", "SCHEME http or https"},
		{"scheme uppercase accepted", "/HTTPS/example.com", PathTarget{"https", "example.com", 0, "/"}, nil, "", ""},

		// --- Scheme-segment policy ({plain, +POLICY} x {http, https}) ---
		{"policy full https", "/https+status=5xx;*.attempts=3;429.attempts=5/example.com/x", PathTarget{"https", "example.com", 0, "/x"}, map[string]string{
			"retry.status": "5xx", "retry[*].attempts": "3", "retry[429].attempts": "5",
		}, "", ""},
		{"policy http with port", "/http+network=1;budget=30s/h.dev:4000/v1/x", PathTarget{"http", "h.dev", 4000, "/v1/x"}, map[string]string{
			"retry.network": "1", "retry.budget": "30s",
		}, "", ""},
		{"policy single gate", "/https+status=429/example.com/x", PathTarget{"https", "example.com", 0, "/x"}, map[string]string{
			"retry.status": "429",
		}, "", ""},
		{"policy key case-sensitive", "/HTTPS+STATUS=429/example.com", PathTarget{}, nil, "unknown policy field", "status"},
		{"policy scheme lowercase with policy", "/Https+status=429/example.com", PathTarget{"https", "example.com", 0, "/"}, map[string]string{
			"retry.status": "429",
		}, "", ""},
		{"policy whitespace around pairs", "/https+ status=429 ; network=0 /example.com", PathTarget{"https", "example.com", 0, "/"}, map[string]string{
			"retry.status": "429", "retry.network": "0",
		}, "", ""},
		{"policy whitespace around equals", "/https+status = 429;network=0/example.com/x", PathTarget{"https", "example.com", 0, "/x"}, map[string]string{
			"retry.status": "429", "retry.network": "0",
		}, "", ""},
		{"policy empty path normalizes", "/https+status=429/example.com", PathTarget{"https", "example.com", 0, "/"}, map[string]string{
			"retry.status": "429",
		}, "", ""},
		{"policy with ipv6", "/https+status=5xx/[2001:db8::1]/x", PathTarget{"https", "2001:db8::1", 0, "/x"}, map[string]string{
			"retry.status": "5xx",
		}, "", ""},
		{"policy raw path preserved", "/https+status=5xx/h/a%2Fb%20c+d", PathTarget{"https", "h", 0, "/a%2Fb%20c+d"}, map[string]string{
			"retry.status": "5xx",
		}, "", ""},
		{"policy values never lowercased", "/https+status=5XX/h", PathTarget{"https", "h", 0, "/"}, map[string]string{
			"retry.status": "5XX",
		}, "", ""},

		// --- Bare-word rule: any bare word is the generic unknown-field 400 ---
		// (any word not in the key set dies identically)
		{"bare word retry", "/https+retry/example.com/x", PathTarget{}, nil, `unknown policy field "retry"`, "status, network, budget"},
		{"bare word pure", "/https+pure/example.com/x", PathTarget{}, nil, `unknown policy field "pure"`, "status, network, budget"},
		{"bare word foo", "/https+foo/example.com/x", PathTarget{}, nil, `unknown policy field "foo"`, "status, network, budget"},
		{"bare word uppercase", "/https+RETRY/example.com/x", PathTarget{}, nil, `unknown policy field "RETRY"`, "status, network, budget"},

		// --- Fail-closed ladder on the segment policy ---
		{"policy key without equals", "/https+status/example.com/x", PathTarget{}, nil, "is not key=value", "key=value"},
		{"empty policy after plus", "/https+/example.com/x", PathTarget{}, nil, "is present but empty", "status=5xx"},
		{"whitespace-only policy", "/https+  /example.com/x", PathTarget{}, nil, "is present but empty", "status=5xx"},
		{"escaped whitespace is not whitespace", "/https+%20%20/example.com/x", PathTarget{}, nil, "unknown policy field", "status, network, budget"},
		{"empty pair in policy", "/https+status=5xx;;network=1/example.com/x", PathTarget{}, nil, "empty pair", "single \";\""},
		{"trailing separator", "/https+status=5xx;/example.com/x", PathTarget{}, nil, "empty pair", "single \";\""},
		{"leading separator", "/https+;status=5xx/example.com/x", PathTarget{}, nil, "empty pair", "single \";\""},
		{"empty key in policy", "/https+=5xx/example.com/x", PathTarget{}, nil, "empty key", "status, network, budget"},
		{"value containing equals", "/https+status=429=500/example.com/x", PathTarget{}, nil, "contains \"=\"", "values never contain"},
		{"unknown gate word", "/https+attempt=3/example.com/x", PathTarget{}, nil, "unknown policy field", "status, network, budget"},
		{"non-digit scope", "/https+42.attempts=2/example.com/x", PathTarget{}, nil, "unknown policy field", "status, network, budget"},
		{"policy without authority", "/https+status=5xx", PathTarget{}, nil, "missing upstream authority", "SCHEME"},

		// --- Scheme-segment grammar edges ---
		{"policy without scheme", "/+status=5xx/example.com/x", PathTarget{}, nil, `unsupported scheme "+status=5xx"`, "SCHEME"},
		{"bare plus segment", "/+/example.com/x", PathTarget{}, nil, `unsupported scheme "+"`, "SCHEME"},
		{"bang separator", "/https!retry/example.com/x", PathTarget{}, nil, `unsupported scheme "https!retry"`, "SCHEME"},
		{"unknown scheme rx", "/rx/example.com/x", PathTarget{}, nil, `unsupported scheme "rx"`, "SCHEME"},
		{"unknown scheme with policy", "/ftp+status=5xx/example.com/x", PathTarget{}, nil, `unsupported scheme "ftp+status=5xx"`, "SCHEME"},
		{"escaped plus is not a policy", "/https%2Bstatus=5xx/example.com/x", PathTarget{}, nil, `unsupported scheme "https%2Bstatus=5xx"`, "SCHEME"},
		{"escaped plus lowercase hex is not a policy", "/https%2bstatus=5xx/example.com/x", PathTarget{}, nil, `unsupported scheme "https%2bstatus=5xx"`, "SCHEME"},

		// --- Valid simple targets (plain scheme segment: no policy) ---
		{"https with path", "/https/api.example.com/v1/chat", PathTarget{"https", "api.example.com", 0, "/v1/chat"}, nil, "", ""},
		{"http with path", "/http/h.dev:4000/v1/x", PathTarget{"http", "h.dev", 4000, "/v1/x"}, nil, "", ""},
		{"empty path normalizes", "/https/example.com", PathTarget{"https", "example.com", 0, "/"}, nil, "", ""},
		{"trailing slash equals root", "/https/example.com/", PathTarget{"https", "example.com", 0, "/"}, nil, "", ""},
		{"scheme case insensitive", "/HTTP/example.com", PathTarget{"http", "example.com", 0, "/"}, nil, "", ""},
		{"deep path preserved", "/https/h/a/b/c/d", PathTarget{"https", "h", 0, "/a/b/c/d"}, nil, "", ""},
		{"empty segment in path preserved", "/https/h/a//b", PathTarget{"https", "h", 0, "/a//b"}, nil, "", ""},
		{"path with colon", "/https/h/a:b", PathTarget{"https", "h", 0, "/a:b"}, nil, "", ""},
		{"path with at-sign", "/https/h/x@y", PathTarget{"https", "h", 0, "/x@y"}, nil, "", ""},
		{"encoded bytes preserved", "/https/h/a%2Fb%20c+d", PathTarget{"https", "h", 0, "/a%2Fb%20c+d"}, nil, "", ""},
		{"percent in path preserved", "/https/h/%zz", PathTarget{"https", "h", 0, "/%zz"}, nil, "", ""},
		{"plus in raw path is not policy", "/https/h/a+b+c", PathTarget{"https", "h", 0, "/a+b+c"}, nil, "", ""},

		// --- Ports ---
		{"explicit https port", "/https/h:8443/x", PathTarget{"https", "h", 8443, "/x"}, nil, "", ""},
		{"port one", "/https/h:1", PathTarget{"https", "h", 1, "/"}, nil, "", ""},
		{"port max", "/https/h:65535", PathTarget{"https", "h", 65535, "/"}, nil, "", ""},
		{"port zero", "/https/h:0", PathTarget{}, nil, "out of range", "1 and 65535"},
		{"port too big", "/https/h:65536", PathTarget{}, nil, "out of range", "1 and 65535"},
		{"port huge", "/https/h:99999", PathTarget{}, nil, "out of range", "1 and 65535"},
		{"port leading zero", "/https/h:080", PathTarget{}, nil, "leading zeros", "leading zeros"},
		{"port leading zero four digits", "/https/h:0080", PathTarget{}, nil, "leading zeros", "leading zeros"},
		{"port non-numeric", "/https/h:8o0", PathTarget{}, nil, "digits only", "plain decimal"},
		{"port negative", "/https/h:-80", PathTarget{}, nil, "digits only", "plain decimal"},
		{"trailing colon empty port", "/https/h:", PathTarget{}, nil, "empty port", "decimal"},
		{"port with letters after digits", "/https/h:80a", PathTarget{}, nil, "digits only", "1-65535"},

		// --- Userinfo forbidden ---
		{"userinfo basic", "/https/user:pass@example.com/", PathTarget{}, nil, "userinfo", "Authorization"},
		{"userinfo bare user", "/https/user@example.com/", PathTarget{}, nil, "userinfo", "Authorization"},
		{"userinfo in path not authority", "/https/h/u@v", PathTarget{"https", "h", 0, "/u@v"}, nil, "", ""},

		// --- IPv4 ---
		{"ipv4 literal", "/http/127.0.0.1:8080/x", PathTarget{"http", "127.0.0.1", 8080, "/x"}, nil, "", ""},
		{"ipv4 default port", "/http/203.0.113.7", PathTarget{"http", "203.0.113.7", 0, "/"}, nil, "", ""},
		{"ipv4 bad octet", "/http/999.0.113.7", PathTarget{}, nil, "invalid IPv4", "octets"},
		{"ipv4 three octets", "/http/1.2.3", PathTarget{}, nil, "invalid IPv4", "octets"},
		{"ipv4 five octets", "/http/1.2.3.4.5", PathTarget{}, nil, "invalid IPv4", "octets"},

		// --- IPv6 (brackets required) ---
		{"ipv6 loopback", "/https/[::1]:8443/x", PathTarget{"https", "::1", 8443, "/x"}, nil, "", ""},
		{"ipv6 no port", "/https/[2001:db8::1]/x", PathTarget{"https", "2001:db8::1", 0, "/x"}, nil, "", ""},
		{"ipv6 full", "/http/[::ffff:192.0.2.1]:80/x", PathTarget{"http", "::ffff:192.0.2.1", 80, "/x"}, nil, "", ""},
		{"ipv6 bare rejected", "/https/::1/x", PathTarget{}, nil, "invalid IPv6", "brackets"},
		{"ipv6 no brackets no port", "/https/2001:db8::1", PathTarget{}, nil, "invalid IPv6", "brackets"},
		{"ipv6 junk after brackets", "/https/[::1]junk", PathTarget{}, nil, "malformed authority", "[addr]:port"},
		{"ipv6 missing close bracket", "/https/[::1/x", PathTarget{}, nil, "missing closing", "[addr]:port"},

		// --- Host shapes ---
		{"dns single label", "/https/localhost", PathTarget{"https", "localhost", 0, "/"}, nil, "", ""},
		{"dns multi label", "/https/a.b.example.com", PathTarget{"https", "a.b.example.com", 0, "/"}, nil, "", ""},
		{"dns with digits", "/https/h1.example.com", PathTarget{"https", "h1.example.com", 0, "/"}, nil, "", ""},
		{"dns internal hyphen", "/https/my-host.example.com", PathTarget{"https", "my-host.example.com", 0, "/"}, nil, "", ""},
		{"dns leading hyphen label", "/https/-host.example.com", PathTarget{}, nil, "invalid host", "letters, digits"},
		{"dns trailing hyphen label", "/https/host-.example.com", PathTarget{}, nil, "invalid host", "letters, digits"},
		{"dns underscore rejected", "/https/my_host.example.com", PathTarget{}, nil, "invalid host", "letters, digits"},
		{"dns empty label double dot", "/https/a..b", PathTarget{}, nil, "empty label", "non-empty"},
		{"dns empty label trailing dot", "/https/a.", PathTarget{}, nil, "empty label", "non-empty"},
		{"dns empty label leading dot", "/https/.a", PathTarget{}, nil, "empty label", "non-empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, params, err := ParsePath(tt.path)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParsePath(%q) = %+v, want error %q", tt.path, got, tt.wantErr)
				}
				if err.Code != 400 {
					t.Errorf("error code = %d, want 400", err.Code)
				}
				if !strings.Contains(err.Reason, tt.wantErr) {
					t.Errorf("error reason = %q, want substring %q", err.Reason, tt.wantErr)
				}
				if tt.wantHnt != "" && !strings.Contains(err.Hint, tt.wantHnt) {
					t.Errorf("error hint = %q, want substring %q", err.Hint, tt.wantHnt)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePath(%q) unexpected error: %v (hint: %s)", tt.path, err, err.Hint)
			}
			if got != tt.want {
				t.Errorf("ParsePath(%q) = %+v, want %+v", tt.path, got, tt.want)
			}
			// Policy params: nil expected ↔ nil returned; map expected ↔ exact match.
			if tt.wantPolicy == nil {
				if params != nil {
					t.Errorf("ParsePath(%q) policy = %v, want nil", tt.path, params)
				}
				return
			}
			if params == nil {
				t.Fatalf("ParsePath(%q) policy = nil, want %v", tt.path, tt.wantPolicy)
			}
			if len(params) != len(tt.wantPolicy) {
				t.Fatalf("policy = %v, want %d keys", params, len(tt.wantPolicy))
			}
			for key, wantVal := range tt.wantPolicy {
				if vals := params[key]; len(vals) != 1 || vals[0] != wantVal {
					t.Errorf("policy[%q] = %v, want [%q]", key, vals, wantVal)
				}
			}
		})
	}
}

// TestParsePathSegmentHeaderGrammarEquivalence: the same policy through the
// scheme segment (dotted scopes) and the policy header (bracketed scopes)
// yields the SAME url.Values — one grammar, two carriers.
func TestParsePathSegmentHeaderGrammarEquivalence(t *testing.T) {
	segment := "/https+status=5xx;network=1;budget=30s;*.attempts=3;429.attempts=5/example.com/x"
	headerVal := "status=5xx; network=1; budget=30s; [*].attempts=3; [429].attempts=5"

	_, viaSegment, rerr := ParsePath(segment)
	if rerr != nil {
		t.Fatalf("ParsePath(%q) error: %v", segment, rerr)
	}
	viaHeader, herr := ParseRetryPolicyHeader(headerFor(RetryPolicyHeader, headerVal))
	if herr != nil {
		t.Fatalf("ParseRetryPolicyHeader error: %v", herr)
	}
	if viaSegment == nil || viaHeader == nil {
		t.Fatalf("nil params: segment=%v header=%v", viaSegment, viaHeader)
	}
	if len(viaSegment) != len(viaHeader) {
		t.Fatalf("key counts differ: segment=%v header=%v", viaSegment, viaHeader)
	}
	for key, vals := range viaHeader {
		got := viaSegment[key]
		if len(got) != 1 || len(vals) != 1 || got[0] != vals[0] {
			t.Errorf("key %q: segment=%v header=%v (must be identical)", key, got, vals)
		}
	}
}

// TestSegmentBracketBijection: the dotted (segment) and bracketed (header)
// scope spellings map to the same retry.* key for every legal scope — the
// mapping is a total bijection, round-trip locked.
func TestSegmentBracketBijection(t *testing.T) {
	cases := []struct {
		segmentKey string
		headerKey  string
		queryKey   string
	}{
		{"*.attempts", "[*].attempts", "retry[*].attempts"},
		{"*.backoff", "[*].backoff", "retry[*].backoff"},
		{"*.initial", "[*].initial", "retry[*].initial"},
		{"*.max", "[*].max", "retry[*].max"},
		{"*.jitter", "[*].jitter", "retry[*].jitter"},
		{"*.retry_after", "[*].retry_after", "retry[*].retry_after"},
		{"100.attempts", "[100].attempts", "retry[100].attempts"},
		{"429.attempts", "[429].attempts", "retry[429].attempts"},
		{"599.attempts", "[599].attempts", "retry[599].attempts"},
		{"status", "status", "retry.status"},
		{"network", "network", "retry.network"},
		{"budget", "budget", "retry.budget"},
	}
	for _, tc := range cases {
		sk, serr := queryKeyForSegmentKey(tc.segmentKey)
		if serr != nil {
			t.Fatalf("segment key %q: %v", tc.segmentKey, serr)
		}
		hk, herr := queryKeyForHeaderKey(tc.headerKey)
		if herr != nil {
			t.Fatalf("header key %q: %v", tc.headerKey, herr)
		}
		if sk != tc.queryKey || hk != tc.queryKey {
			t.Errorf("bijection broken: segment %q -> %q (want %q), header %q -> %q (want %q)",
				tc.segmentKey, sk, tc.queryKey, tc.headerKey, hk, tc.queryKey)
		}
	}

	// Round trip through the full parse for both carriers: each spelling of
	// the same policy resolves to the same Policy.
	for _, pair := range [][2]string{
		{"status=5xx;*.attempts=3", "status=5xx; [*].attempts=3"},
		{"status=429,5xx;429.initial=100ms", "status=429,5xx; [429].initial=100ms"},
	} {
		_, seg, rerr := ParsePath("/https+" + pair[0] + "/h")
		if rerr != nil {
			t.Fatalf("segment %q: %v", pair[0], rerr)
		}
		hdr, herr := ParseRetryPolicyHeader(headerFor(RetryPolicyHeader, pair[1]))
		if herr != nil {
			t.Fatalf("header %q: %v", pair[1], herr)
		}
		ps, e1 := Parse(seg, NewDefaultConfig())
		ph, e2 := Parse(hdr, NewDefaultConfig())
		if e1 != nil || e2 != nil {
			t.Fatalf("Parse errors: segment=%v header=%v", e1, e2)
		}
		if !reflect.DeepEqual(ps, ph) {
			t.Errorf("policies differ for %q: segment=%+v header=%+v", pair[0], ps, ph)
		}
	}
}

func TestPathTargetNormalizeAndRendering(t *testing.T) {
	tests := []struct {
		name         string
		in           PathTarget
		wantPort     int
		wantHostPort string
		wantURL      string
	}{
		{"http default", PathTarget{"http", "h", 0, "/"}, 80, "h:80", "http://h:80/"},
		{"https default", PathTarget{"https", "h", 0, "/"}, 443, "h:443", "https://h:443/"},
		{"explicit kept", PathTarget{"https", "h", 8443, "/x"}, 8443, "h:8443", "https://h:8443/x"},
		{"ipv6 bracketed in hostport", PathTarget{"https", "::1", 0, "/"}, 443, "[::1]:443", "https://[::1]:443/"},
		{"root path elided in url", PathTarget{"https", "h", 0, "/"}, 443, "h:443", "https://h:443/"},
		{"raw path preserved in url", PathTarget{"https", "h", 0, "/a%2Fb"}, 443, "h:443", "https://h:443/a%2Fb"},
		// The policy is orthogonal to destination rendering: HostPort/URL/
		// Normalize behave identically with or without a segment policy.
		{"explicit kept with policy carrier", PathTarget{"http", "h", 8080, "/x"}, 8080, "h:8080", "http://h:8080/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := tt.in.Normalize()
			if n.Port != tt.wantPort {
				t.Errorf("Normalize() port = %d, want %d", n.Port, tt.wantPort)
			}
			if hp := n.HostPort(); hp != tt.wantHostPort {
				t.Errorf("HostPort() = %q, want %q", hp, tt.wantHostPort)
			}
			if u := n.URL(); u != tt.wantURL {
				t.Errorf("URL() = %q, want %q", u, tt.wantURL)
			}
		})
	}
}
