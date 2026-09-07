package reproxy

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// parseForTest is a helper: url.Values in, Policy out, fatal on error.
func parseForTest(t *testing.T, params url.Values) Policy {
	t.Helper()
	p, err := Parse(params, NewDefaultConfig())
	if err != nil {
		t.Fatalf("Parse(%v) error: %v", params, err)
	}
	return p
}

func params(pairs ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v.Set(pairs[i], pairs[i+1])
	}
	return v
}

func TestParseDefaults(t *testing.T) {
	p := parseForTest(t, url.Values{})
	want := ScopePolicy{
		Attempts:   3,
		Backoff:    "exponential",
		Initial:    1 * time.Second,
		Max:        8 * time.Second,
		Jitter:     "full",
		RetryAfter: "honor",
	}
	if p.Default != want {
		t.Errorf("Default = %+v, want %+v", p.Default, want)
	}
	if len(p.StatusGate) != 0 {
		t.Errorf("StatusGate = %v, want empty (no status-based retry)", p.StatusGate)
	}
	if !p.NetworkGate {
		t.Errorf("NetworkGate = false, want true")
	}
	if p.Budget != 30*time.Second {
		t.Errorf("Budget = %s, want 30s", p.Budget)
	}
	if len(p.ByStatus) != 0 {
		t.Errorf("ByStatus = %v, want empty", p.ByStatus)
	}
	if p.Effective(500) != want {
		t.Errorf("Effective(500) with no overrides = %+v, want default", p.Effective(500))
	}
}

func TestParseThreeTierPriority(t *testing.T) {
	// Built-in default -> retry[*] override -> retry[NNN] field-level override.
	p := parseForTest(t, params(
		"retry.status", "429",
		"retry[*].attempts", "5",
		"retry[*].initial", "2s",
		"retry[*].jitter", "none",
		"retry[429].attempts", "7",
	))
	if p.Default.Attempts != 5 {
		t.Errorf("Default.Attempts = %d, want 5 (from retry[*])", p.Default.Attempts)
	}
	if p.Default.Initial != 2*time.Second {
		t.Errorf("Default.Initial = %s, want 2s (from retry[*])", p.Default.Initial)
	}
	if p.Default.Jitter != "none" {
		t.Errorf("Default.Jitter = %q, want none (from retry[*])", p.Default.Jitter)
	}
	if p.Default.Backoff != "exponential" {
		t.Errorf("Default.Backoff = %q, want exponential (built-in, not overridden)", p.Default.Backoff)
	}
	sp := p.Effective(429)
	if sp.Attempts != 7 {
		t.Errorf("Effective(429).Attempts = %d, want 7 (from retry[429])", sp.Attempts)
	}
	if sp.Initial != 2*time.Second {
		t.Errorf("Effective(429).Initial = %s, want 2s (inherited from retry[*])", sp.Initial)
	}
	if sp.Jitter != "none" {
		t.Errorf("Effective(429).Jitter = %q, want none (inherited from retry[*])", sp.Jitter)
	}
	if sp.Backoff != "exponential" {
		t.Errorf("Effective(429).Backoff = %q, want exponential (inherited)", sp.Backoff)
	}
	// A status without a scope falls back to Default.
	if p.Effective(500).Attempts != 5 {
		t.Errorf("Effective(500).Attempts = %d, want 5 (default scope)", p.Effective(500).Attempts)
	}
}

func TestParseScopeMergeFullPolicy(t *testing.T) {
	// Every ByStatus entry must be a full ScopePolicy after merging.
	p := parseForTest(t, params(
		"retry.status", "500",
		"retry[*].attempts", "4",
		"retry[*].backoff", "linear",
		"retry[*].initial", "500ms",
		"retry[*].max", "4s",
		"retry[*].jitter", "equal",
		"retry[*].retry_after", "ignore",
		"retry[500].attempts", "2",
	))
	want := ScopePolicy{
		Attempts:   2,
		Backoff:    "linear",
		Initial:    500 * time.Millisecond,
		Max:        4 * time.Second,
		Jitter:     "equal",
		RetryAfter: "ignore",
	}
	if got := p.Effective(500); got != want {
		t.Errorf("Effective(500) = %+v, want %+v", got, want)
	}
}

func TestParseServerClamp(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.MaxAttempts = 4
	cfg.MaxBudget = 10 * time.Second

	p, err := Parse(params(
		"retry.status", "429",
		"retry[*].attempts", "999",
		"retry[429].attempts", "888",
		"retry.budget", "10m",
	), cfg)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if p.Default.Attempts != 4 {
		t.Errorf("Default.Attempts = %d, want 4 (clamped from 999)", p.Default.Attempts)
	}
	if got := p.Effective(429).Attempts; got != 4 {
		t.Errorf("Effective(429).Attempts = %d, want 4 (clamped from 888)", got)
	}
	if p.Budget != 10*time.Second {
		t.Errorf("Budget = %s, want 10s (clamped from 10m)", p.Budget)
	}

	// Clamp narrows, never widens: attempts below the cap stay untouched.
	p2, err := Parse(params("retry[*].attempts", "2"), cfg)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if p2.Default.Attempts != 2 {
		t.Errorf("Default.Attempts = %d, want 2 (below cap, untouched)", p2.Default.Attempts)
	}
}

func TestParseDuplicateKeysLastWins(t *testing.T) {
	v := url.Values{}
	v.Add("retry[*].attempts", "9")
	v.Add("retry[*].attempts", "2")
	p := parseForTest(t, v)
	if p.Default.Attempts != 2 {
		t.Errorf("Default.Attempts = %d, want 2 (last wins)", p.Default.Attempts)
	}
}

func TestParseStatusGate(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []int
	}{
		{"single code", "429", []int{429}},
		{"list", "400,429,500", []int{400, 429, 500}},
		{"range", "500-599", []int{500, 599}}, // endpoints only for brevity; checked fully below
		{"class lower", "5xx", []int{500, 501, 502, 503, 504, 505, 506, 507, 508, 509, 510, 511, 512, 513, 514, 515, 516, 517, 518, 519, 520, 521, 522, 523, 524, 525, 526, 527, 528, 529, 530, 531, 532, 533, 534, 535, 536, 537, 538, 539, 540, 541, 542, 543, 544, 545, 546, 547, 548, 549, 550, 551, 552, 553, 554, 555, 556, 557, 558, 559, 560, 561, 562, 563, 564, 565, 566, 567, 568, 569, 570, 571, 572, 573, 574, 575, 576, 577, 578, 579, 580, 581, 582, 583, 584, 585, 586, 587, 588, 589, 590, 591, 592, 593, 594, 595, 596, 597, 598, 599}},
		{"class upper", "5XX", nil},     // same as 5xx; compared via RetryableStatus below
		{"mixed forms", "429,5xx", nil}, // checked below
		{"duplicate dedupe", "429,429,429", []int{429}},
		{"overlap union", "500-599,503", []int{500, 599}}, // spot check + full below
		{"range single", "429-429", []int{429}},
		{"min code", "100", []int{100}},
		{"max code", "599", []int{599}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Parse(params("retry.status", tt.value), NewDefaultConfig())
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			if tt.want != nil {
				// For ranges, verify endpoints + count rather than full lists.
				if tt.name == "range" || tt.name == "overlap union" {
					if len(p.StatusGate) != 100 {
						t.Errorf("StatusGate length = %d, want 100", len(p.StatusGate))
					}
					if p.StatusGate[0] != tt.want[0] || p.StatusGate[len(p.StatusGate)-1] != tt.want[1] {
						t.Errorf("StatusGate endpoints = %d..%d, want %d..%d",
							p.StatusGate[0], p.StatusGate[len(p.StatusGate)-1], tt.want[0], tt.want[1])
					}
					return
				}
				if len(p.StatusGate) != len(tt.want) {
					t.Fatalf("StatusGate = %v, want %v", p.StatusGate, tt.want)
				}
				for i := range tt.want {
					if p.StatusGate[i] != tt.want[i] {
						t.Errorf("StatusGate[%d] = %d, want %d", i, p.StatusGate[i], tt.want[i])
					}
				}
				return
			}
			// Class-shorthand cases: verify via RetryableStatus.
			if tt.value == "5XX" || tt.value == "429,5xx" {
				if !p.RetryableStatus(503) {
					t.Errorf("RetryableStatus(503) = false for %q", tt.value)
				}
				if p.RetryableStatus(499) {
					t.Errorf("RetryableStatus(499) = true for %q, want false", tt.value)
				}
			}
			if tt.value == "429,5xx" && !p.RetryableStatus(429) {
				t.Errorf("RetryableStatus(429) = false for %q", tt.value)
			}
		})
	}
}

func TestParseStatusGateInvalid(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"empty string", "", "at least one"},
		{"empty item", "429,,500", "empty item"},
		{"non-numeric", "abc", "invalid status code"},
		{"two digits", "42", "invalid status code"},
		{"four digits", "4299", "invalid status code"},
		{"too high", "600", "out of range"},
		{"too low", "099", "out of range"},
		{"whitespace in list", "500 -599", "invalid status code"},
		{"whitespace around code", " 429", "invalid status code"},
		{"reversed range", "599-500", "reversed"},
		{"open range high", "500-", "invalid status code"},
		{"open range low", "-500", "invalid status code"},
		{"bad range bound", "500-abc", "invalid status code"},
		{"bad class", "6xx", "invalid status code"},
		{"class not padded", "5x", "invalid status code"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(params("retry.status", tt.value), NewDefaultConfig())
			if err == nil {
				t.Fatalf("Parse with retry.status=%q should fail", tt.value)
			}
			if err.Code != 400 {
				t.Errorf("code = %d, want 400", err.Code)
			}
			if !strings.Contains(err.Reason, tt.want) {
				t.Errorf("reason = %q, want substring %q", err.Reason, tt.want)
			}
			if err.Hint == "" {
				t.Errorf("hint should not be empty")
			}
		})
	}
}

func TestParseNetworkGate(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"0", false},
		{"1", true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			p := parseForTest(t, params("retry.network", tt.value))
			if p.NetworkGate != tt.want {
				t.Errorf("NetworkGate = %v, want %v", p.NetworkGate, tt.want)
			}
		})
	}
	// Default is true.
	if !parseForTest(t, url.Values{}).NetworkGate {
		t.Errorf("NetworkGate default = false, want true")
	}
	// A valueless or empty value is a client error (fail closed): the
	// documented contract accepts only "0" or "1".
	for _, bad := range []string{"", "true", "on"} {
		_, err := Parse(params("retry.network", bad), NewDefaultConfig())
		if err == nil || err.Code != 400 {
			t.Fatalf("retry.network=%q should be a 400, got %v", bad, err)
		}
	}
	_, err := Parse(params("retry.network", "2"), NewDefaultConfig())
	if err == nil || err.Code != 400 {
		t.Fatalf("retry.network=2 should be a 400, got %v", err)
	}
	_, err = Parse(params("retry.network", "yes"), NewDefaultConfig())
	if err == nil || err.Code != 400 {
		t.Fatalf("retry.network=yes should be a 400, got %v", err)
	}
}

func TestParseBudget(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.MaxBudget = 5 * time.Minute // allow values above the default cap

	p, err := Parse(params("retry.budget", "90s"), cfg)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if p.Budget != 90*time.Second {
		t.Errorf("Budget = %s, want 90s", p.Budget)
	}
	p, err = Parse(params("retry.budget", "1m30s"), cfg)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if p.Budget != 90*time.Second {
		t.Errorf("Budget = %s, want 1m30s", p.Budget)
	}
	_, err = Parse(params("retry.budget", "30"), NewDefaultConfig())
	if err == nil || err.Code != 400 {
		t.Fatalf("retry.budget=30 (bare number) should be a 400, got %v", err)
	}
	if !strings.Contains(err.Reason, "retry.budget") {
		t.Errorf("reason %q should name retry.budget", err.Reason)
	}
	_, err = Parse(params("retry.budget", "0s"), NewDefaultConfig())
	if err == nil || err.Code != 400 {
		t.Fatalf("retry.budget=0s should be a 400, got %v", err)
	}
}

func TestParseInvalidScopeFields(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"attempts non-integer", "retry[*].attempts", "abc"},
		{"attempts zero", "retry[*].attempts", "0"},
		{"attempts negative", "retry[*].attempts", "-1"},
		{"attempts float", "retry[*].attempts", "1.5"},
		{"backoff unknown", "retry[*].backoff", "foo"},
		{"jitter unknown", "retry[*].jitter", "foo"},
		{"retry_after unknown", "retry[*].retry_after", "foo"},
		{"initial bare number", "retry[*].initial", "100"},
		{"initial garbage", "retry[*].initial", "fast"},
		{"max bare number", "retry[*].max", "8"},
		{"max below initial", "retry[*].max", "500ms"}, // default initial is 1s; below it fails
		{"status scope attempts zero", "retry[429].attempts", "0"},
		{"status scope backoff unknown", "retry[429].backoff", "foo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := params(tt.key, tt.value)
			if strings.HasPrefix(tt.key, "retry[429]") {
				values.Set("retry.status", "429")
			}
			_, err := Parse(values, NewDefaultConfig())
			if err == nil {
				t.Fatalf("Parse with %s=%s should fail", tt.key, tt.value)
			}
			if err.Code != 400 {
				t.Errorf("code = %d, want 400", err.Code)
			}
			if !strings.Contains(err.Reason, tt.key) {
				t.Errorf("reason %q should name the offending key %q", err.Reason, tt.key)
			}
			if err.Hint == "" {
				t.Errorf("hint should not be empty")
			}
		})
	}
}

func TestParseMaxInitialRelationship(t *testing.T) {
	// max == initial is fine; max < initial is a 400 at the exact scope where
	// it becomes true.
	p := parseForTest(t, params(
		"retry[*].initial", "2s",
		"retry[*].max", "2s",
	))
	if p.Default.Initial != 2*time.Second || p.Default.Max != 2*time.Second {
		t.Errorf("initial==max should be accepted, got %+v", p.Default)
	}

	_, err := Parse(params(
		"retry.status", "429",
		"retry[*].initial", "5s",
		"retry[429].max", "4s",
	), NewDefaultConfig())
	if err == nil || err.Code != 400 {
		t.Fatalf("retry[429].max < inherited initial should be a 400, got %v", err)
	}
}

func TestParseGateInStatusScope(t *testing.T) {
	// Gates are global: retry[NNN].status / .network / .budget cannot appear.
	// These keys are unknown at the scope level (they are not scopeFields);
	// Parse must fail closed if handed such params directly.
	for _, key := range []string{
		"retry[429].status",
		"retry[429].network",
		"retry[429].budget",
		"retry[429].bogus",
	} {
		t.Run(key, func(t *testing.T) {
			_, err := Parse(url.Values{
				"retry.status": {"429"},
				key:            {"1"},
			}, NewDefaultConfig())
			if err == nil {
				t.Fatalf("%s should be rejected (gates and fields are scoped differently)", key)
			}
			if err.Code != 400 {
				t.Errorf("code = %d, want 400", err.Code)
			}
			if !strings.Contains(err.Reason, key) {
				t.Errorf("reason %q should name the key %q", err.Reason, key)
			}
		})
	}
}

func TestParseDeadConfig(t *testing.T) {
	// retry[429].attempts=4 but 429 not in retry.status -> 400.
	_, err := Parse(params(
		"retry[*].attempts", "3",
		"retry[429].attempts", "4",
	), NewDefaultConfig())
	if err == nil {
		t.Fatalf("dead config (429 not in retry.status) should be a 400")
	}
	if err.Code != 400 {
		t.Errorf("code = %d, want 400", err.Code)
	}
	if !strings.Contains(err.Reason, "dead retry configuration") {
		t.Errorf("reason = %q, want dead-config message", err.Reason)
	}
	if !strings.Contains(err.Reason, "429") {
		t.Errorf("reason %q should name the status code 429", err.Reason)
	}

	// Same scope, code IN the gate: fine.
	p := parseForTest(t, params(
		"retry.status", "429",
		"retry[429].attempts", "4",
	))
	if got := p.Effective(429).Attempts; got != 4 {
		t.Errorf("Effective(429).Attempts = %d, want 4", got)
	}
	if !p.RetryableStatus(429) {
		t.Errorf("RetryableStatus(429) = false, want true")
	}

	// Gate covers the code via a class shorthand: also fine.
	p2 := parseForTest(t, params(
		"retry.status", "429,5xx",
		"retry[503].attempts", "2",
	))
	if !p2.RetryableStatus(503) {
		t.Errorf("RetryableStatus(503) = false, want true (5xx class)")
	}
}

func TestParseNilConfig(t *testing.T) {
	// cfg=nil means no server clamps; parsing still works.
	p, err := Parse(params(
		"retry.status", "429",
		"retry[*].attempts", "999",
	), nil)
	if err != nil {
		t.Fatalf("Parse with nil cfg error: %v", err)
	}
	if p.Default.Attempts != 999 {
		t.Errorf("Default.Attempts = %d, want 999 (no clamp with nil cfg)", p.Default.Attempts)
	}
}

func TestEffectiveFallback(t *testing.T) {
	p := parseForTest(t, params(
		"retry.status", "429,500",
		"retry[429].attempts", "2",
	))
	if got := p.Effective(500); got.Attempts != 3 {
		t.Errorf("Effective(500).Attempts = %d, want 3 (default)", got.Attempts)
	}
	if got := p.Effective(502); got.Attempts != 3 {
		t.Errorf("Effective(502).Attempts = %d, want 3 (default, not in gate)", got.Attempts)
	}
}

// TestParseFieldOrderIndependence pins the fix for the order-dependence bug:
// the max<initial cross-field check ran per-field while url.Values iterates
// in random map order, so a final-valid config (initial=10s with max=20s)
// was rejected whenever max was applied first. The check now runs after
// each scope is fully assembled.
func TestParseFieldOrderIndependence(t *testing.T) {
	for i := 0; i < 50; i++ {
		p, err := Parse(url.Values{
			"retry[*].initial": {"10s"},
			"retry[*].max":     {"20s"},
		}, nil)
		if err != nil {
			t.Fatalf("iter %d: final-valid config rejected: %v", i, err)
		}
		if p.Default.Initial != 10*time.Second || p.Default.Max != 20*time.Second {
			t.Fatalf("iter %d: initial=%s max=%s", i, p.Default.Initial, p.Default.Max)
		}
	}
	// Same for a status scope: [429] raising initial above an inherited max.
	for i := 0; i < 50; i++ {
		p, err := Parse(url.Values{
			"retry.status":       {"429"},
			"retry[*].max":       {"30s"},
			"retry[429].initial": {"10s"},
			"retry[429].max":     {"20s"},
		}, nil)
		if err != nil {
			t.Fatalf("iter %d: final-valid status scope rejected: %v", i, err)
		}
		if sp := p.ByStatus[429]; sp.Initial != 10*time.Second || sp.Max != 20*time.Second {
			t.Fatalf("iter %d: [429] initial=%s max=%s", i, sp.Initial, sp.Max)
		}
	}
	// And the genuinely-invalid case still fails deterministically.
	_, err := Parse(url.Values{
		"retry.status":     {"429"},
		"retry[*].initial": {"5s"},
		"retry[429].max":   {"4s"},
	}, nil)
	if err == nil || err.Code != 400 {
		t.Fatalf("max < initial must still be a 400, got %v", err)
	}
}
