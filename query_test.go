package reproxy

import (
	"strings"
	"testing"
)

func TestSplitQueryPassthroughBytePreservation(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"percent encoded slash", "a=%2Fb&c=d", "a=%2Fb&c=d"},
		{"percent encoded space", "a=b%20c", "a=b%20c"},
		{"plus preserved", "a=b+c", "a=b+c"},
		{"plus and percent mix", "q=a+b%2Bc%20d", "q=a+b%2Bc%20d"},
		{"no value keys", "a&b", "a&b"},
		{"empty value key", "a=", "a="},
		{"duplicate keys keep order", "a=1&b=2&a=3", "a=1&b=2&a=3"},
		{"mixed no-value and valued", "a&b=1&c&d=", "a&b=1&c&d="},
		{"empty segments preserved", "a=1&&b=2", "a=1&&b=2"},
		{"trailing ampersand", "a=1&", "a=1&"},
		{"leading ampersand", "&a=1", "&a=1"},
		{"raw percent sequence untouched", "x=%zz&y=%2", "x=%zz&y=%2"},
		{"key with encoded chars", "%61=%62", "%61=%62"},
		{"long mixed query", "z=9&a=%2F&flag&b=+&c=%20&d=&&e", "z=9&a=%2F&flag&b=+&c=%20&d=&&e"},
		{"semicolon not a separator", "a=1;b=2", "a=1;b=2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, retry, err := SplitQuery(tt.in)
			if err != nil {
				t.Fatalf("SplitQuery(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("upstreamQuery = %q, want %q", got, tt.want)
			}
			if len(retry) != 0 {
				t.Errorf("retryParams should be empty, got %v", retry)
			}
		})
	}
}

func TestSplitQueryRetryNamespaceStripped(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"gate alone", "retry.status=429,5xx", ""},
		{"network gate", "retry.network=0", ""},
		{"budget gate", "retry.budget=10s", ""},
		{"default scope", "retry[*].attempts=5", ""},
		{"status scope", "retry[429].attempts=4", ""},
		{"mixed passthrough", "a=1&retry.status=500&b=2", "a=1&b=2"},
		{"retry before and after", "retry[*].initial=1s&keep=me&retry[500].max=2s", "keep=me"},
		{"multiple retry keys", "retry.status=429&retry[*].attempts=3&x=y&retry[429].jitter=none", "x=y"},
		{"valueless retry key", "retry.status", ""},
		{"retry key empty value", "retry.network=", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := SplitQuery(tt.in)
			if err != nil {
				t.Fatalf("SplitQuery(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("upstreamQuery = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSplitQueryRetryParamsCollected(t *testing.T) {
	in := "a=1&retry.status=429,500-599&retry[*].attempts=3&retry[429].attempts=4&retry.network=0&retry.budget=30s"
	upstream, got, err := SplitQuery(in)
	if err != nil {
		t.Fatalf("SplitQuery error: %v", err)
	}
	if upstream != "a=1" {
		t.Errorf("upstreamQuery = %q, want %q", upstream, "a=1")
	}
	want := map[string]string{
		"retry.status":        "429,500-599",
		"retry[*].attempts":   "3",
		"retry[429].attempts": "4",
		"retry.network":       "0",
		"retry.budget":        "30s",
	}
	for key, wantVal := range want {
		vals := got[key]
		if len(vals) != 1 {
			t.Errorf("retryParams[%q] = %v, want exactly one value", key, vals)
			continue
		}
		if vals[0] != wantVal {
			t.Errorf("retryParams[%q] = %q, want %q", key, vals[0], wantVal)
		}
	}
}

func TestSplitQueryDecodedRetryValues(t *testing.T) {
	// Retry-namespace values are decoded (they are re-validated later);
	// passthrough bytes stay untouched.
	upstream, got, err := SplitQuery("keep=%2F&retry[*].initial=100%6Ds")
	if err != nil {
		t.Fatalf("SplitQuery error: %v", err)
	}
	if upstream != "keep=%2F" {
		t.Errorf("upstreamQuery = %q", upstream)
	}
	if v := got.Get("retry[*].initial"); v != "100ms" {
		t.Errorf("decoded retry value = %q, want %q", v, "100ms")
	}
}

func TestSplitQueryNotInNamespace(t *testing.T) {
	tests := []string{
		"retryfoo=1",
		"myretry.x=1",
		"retry=1",
		"retryx.status=1",
		"xretry.status=1",
		"retryy[*].attempts=1",
		"a.retry.status=1",
	}
	for _, key := range tests {
		t.Run(key, func(t *testing.T) {
			got, retry, err := SplitQuery(key)
			if err != nil {
				t.Fatalf("SplitQuery(%q) error: %v", key, err)
			}
			if got != key {
				t.Errorf("upstreamQuery = %q, want passthrough %q", got, key)
			}
			if len(retry) != 0 {
				t.Errorf("retryParams = %v, want empty", retry)
			}
		})
	}
}

func TestSplitQueryUnknownRetryKeysRejected(t *testing.T) {
	tests := []struct {
		key     string
		wantMsg string
	}{
		{"retry.attempt=1", "unknown retry parameter"},
		{"retry.foo=1", "unknown retry parameter"},
		{"retry[*].attempt=1", "unknown retry parameter"},
		{"retry[*].bogus=1", "unknown retry parameter"},
		{"retry[abc].x=1", "invalid retry scope"},
		{"retry[999].x=1", "unknown retry parameter"}, // 999 not a status; shape digits, field bogus -> unknown field first
		{"retry[42].attempts=1", "invalid retry scope"},
		{"retry[4294].attempts=1", "invalid retry scope"},
		{"retry[].attempts=1", "invalid retry scope"},
		{"retry[429]attempts=1", "malformed retry parameter"},
		{"retry[429].=1", "malformed retry parameter"},
		{"retry[429]=1", "malformed retry parameter"},
		{"retry[*attempts=1", "malformed retry parameter"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			_, _, err := SplitQuery(tt.key)
			if err == nil {
				t.Fatalf("SplitQuery(%q) should fail", tt.key)
			}
			if err.Code != 400 {
				t.Errorf("code = %d, want 400", err.Code)
			}
			if !strings.Contains(err.Reason, tt.wantMsg) {
				t.Errorf("reason = %q, want substring %q", err.Reason, tt.wantMsg)
			}
			// Error must name the offending key.
			if !strings.Contains(err.Reason, tt.key[:strings.Index(tt.key, "=")]) {
				t.Errorf("reason %q does not name key %q", err.Reason, tt.key)
			}
			if err.Hint == "" {
				t.Errorf("hint should not be empty for %q", tt.key)
			}
		})
	}
}

func TestSplitQueryEmpty(t *testing.T) {
	got, retry, err := SplitQuery("")
	if err != nil {
		t.Fatalf("SplitQuery(\"\") error: %v", err)
	}
	if got != "" || len(retry) != 0 {
		t.Errorf("SplitQuery(\"\") = %q, %v", got, retry)
	}
}
