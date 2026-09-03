package reproxy

import (
	"math/rand/v2"
	"net/http"
	"testing"
	"time"
)

func TestComputeWaitExponentialSequence(t *testing.T) {
	sp := ScopePolicy{
		Attempts:   10,
		Backoff:    "exponential",
		Initial:    1 * time.Second,
		Max:        8 * time.Second,
		Jitter:     "none",
		RetryAfter: "ignore",
	}
	want := []time.Duration{
		1 * time.Second, // retry 1: 1s x 2^0
		2 * time.Second, // retry 2: 1s x 2^1
		4 * time.Second, // retry 3: 1s x 2^2
		8 * time.Second, // retry 4: 1s x 2^3
		8 * time.Second, // retry 5: capped at max
		8 * time.Second, // retry 6: capped at max
	}
	for i, w := range want {
		got := ComputeWait(sp, i+1, "", 10*time.Minute, time.Now())
		if got != w {
			t.Errorf("ComputeWait(retry %d) = %s, want %s", i+1, got, w)
		}
	}
}

func TestComputeWaitLinear(t *testing.T) {
	sp := ScopePolicy{
		Attempts:   10,
		Backoff:    "linear",
		Initial:    1 * time.Second,
		Max:        8 * time.Second,
		Jitter:     "none",
		RetryAfter: "ignore",
	}
	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		3 * time.Second,
		4 * time.Second,
		5 * time.Second, // 5s is still within max 8s; the cap binds from retry 9
	}
	// linear: 1,2,3,4,5,6,7,8,8(capped at 9th)
	for n := 1; n <= 9; n++ {
		got := ComputeWait(sp, n, "", 10*time.Minute, time.Now())
		expected := time.Duration(n) * time.Second
		if expected > 8*time.Second {
			expected = 8 * time.Second
		}
		if got != expected {
			t.Errorf("ComputeWait(linear, retry %d) = %s, want %s", n, got, expected)
		}
	}
	// Sanity-check the table above (kept for readability of intent).
	for i, w := range want {
		if got := ComputeWait(sp, i+1, "", 10*time.Minute, time.Now()); got != w {
			t.Errorf("table check retry %d = %s, want %s", i+1, got, w)
		}
	}
}

func TestComputeWaitConstant(t *testing.T) {
	sp := ScopePolicy{
		Attempts:   10,
		Backoff:    "constant",
		Initial:    2 * time.Second,
		Max:        8 * time.Second,
		Jitter:     "none",
		RetryAfter: "ignore",
	}
	for n := 1; n <= 5; n++ {
		got := ComputeWait(sp, n, "", 10*time.Minute, time.Now())
		if got != 2*time.Second {
			t.Errorf("ComputeWait(constant, retry %d) = %s, want 2s", n, got)
		}
	}
}

func TestComputeWaitJitterFullBounds(t *testing.T) {
	sp := ScopePolicy{
		Attempts:   10,
		Backoff:    "constant",
		Initial:    4 * time.Second,
		Max:        4 * time.Second,
		Jitter:     "full",
		RetryAfter: "ignore",
	}
	for i := 0; i < 200; i++ {
		got := ComputeWait(sp, 1, "", 10*time.Minute, time.Now())
		if got < 0 || got > 4*time.Second {
			t.Fatalf("full jitter wait %s out of [0, 4s]", got)
		}
	}
	// With the full range available, we should see variety (not all identical).
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		seen[ComputeWait(sp, 1, "", 10*time.Minute, time.Now())] = true
	}
	if len(seen) < 10 {
		t.Errorf("full jitter produced only %d distinct values over 200 samples; want variety", len(seen))
	}
}

func TestComputeWaitJitterEqualBounds(t *testing.T) {
	sp := ScopePolicy{
		Attempts:   10,
		Backoff:    "constant",
		Initial:    4 * time.Second,
		Max:        4 * time.Second,
		Jitter:     "equal",
		RetryAfter: "ignore",
	}
	for i := 0; i < 200; i++ {
		got := ComputeWait(sp, 1, "", 10*time.Minute, time.Now())
		if got < 2*time.Second || got > 4*time.Second {
			t.Fatalf("equal jitter wait %s out of [2s, 4s]", got)
		}
	}
}

func TestComputeWaitRetryAfter(t *testing.T) {
	sp := ScopePolicy{
		Attempts:   10,
		Backoff:    "exponential",
		Initial:    1 * time.Second,
		Max:        8 * time.Second,
		Jitter:     "none",
		RetryAfter: "honor",
	}
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"seconds replace computed, capped at max", "120", 8 * time.Second},
		{"seconds below max pass through", "4", 4 * time.Second},
		{"future http-date within max", now.Add(30 * time.Second).Format(http.TimeFormat), 8 * time.Second},
		{"past http-date is zero", now.Add(-time.Hour).Format(http.TimeFormat), 0},
		{"unparseable falls back to computed", "abc", 1 * time.Second},
		{"empty falls back to computed", "", 1 * time.Second},
		{"huge value capped at max", "3600", 8 * time.Second},
		{"zero seconds", "0", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeWait(sp, 1, tt.header, 10*time.Minute, now)
			if got != tt.want {
				t.Errorf("ComputeWait(header %q) = %s, want %s", tt.header, got, tt.want)
			}
		})
	}
}

func TestComputeWaitRetryAfterIgnored(t *testing.T) {
	sp := ScopePolicy{
		Attempts:   10,
		Backoff:    "constant",
		Initial:    1 * time.Second,
		Max:        8 * time.Second,
		Jitter:     "none",
		RetryAfter: "ignore",
	}
	got := ComputeWait(sp, 1, "120", 10*time.Minute, time.Now())
	if got != 1*time.Second {
		t.Errorf("ignore mode should not use Retry-After: got %s, want 1s", got)
	}
}

func TestComputeWaitBudgetCap(t *testing.T) {
	sp := ScopePolicy{
		Attempts:   10,
		Backoff:    "exponential",
		Initial:    1 * time.Second,
		Max:        8 * time.Second,
		Jitter:     "none",
		RetryAfter: "ignore",
	}
	// Computed wait 8s (retry 4) but only 2s of budget left.
	got := ComputeWait(sp, 4, "", 2*time.Second, time.Now())
	if got != 2*time.Second {
		t.Errorf("ComputeWait with 2s budget = %s, want 2s", got)
	}
	// Zero remaining budget clamps to zero.
	got = ComputeWait(sp, 1, "", 0, time.Now())
	if got != 0 {
		t.Errorf("ComputeWait with 0 budget = %s, want 0", got)
	}
	// Negative (already over budget) clamps to zero, not negative.
	got = ComputeWait(sp, 1, "", -5*time.Second, time.Now())
	if got != 0 {
		t.Errorf("ComputeWait with negative budget = %s, want 0", got)
	}
}

func TestComputeWaitAttemptClamped(t *testing.T) {
	sp := ScopePolicy{
		Attempts:   10,
		Backoff:    "exponential",
		Initial:    1 * time.Second,
		Max:        8 * time.Second,
		Jitter:     "none",
		RetryAfter: "ignore",
	}
	// attempt 0 or negative behaves as the first retry.
	if got := ComputeWait(sp, 0, "", time.Hour, time.Now()); got != 1*time.Second {
		t.Errorf("ComputeWait(attempt 0) = %s, want 1s (treated as first retry)", got)
	}
	if got := ComputeWait(sp, -3, "", time.Hour, time.Now()); got != 1*time.Second {
		t.Errorf("ComputeWait(attempt -3) = %s, want 1s (treated as first retry)", got)
	}
	// Very large attempt saturates at max without overflowing.
	if got := ComputeWait(sp, 1000, "", time.Hour, time.Now()); got != 8*time.Second {
		t.Errorf("ComputeWait(attempt 1000) = %s, want 8s (saturated)", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		value  string
		want   time.Duration
		wantOK bool
	}{
		{"plain seconds", "120", 120 * time.Second, true},
		{"zero", "0", 0, true},
		{"negative clamped", "-5", 0, true},
		{"http-date future", now.Add(time.Minute).Format(http.TimeFormat), time.Minute - time.Duration(now.Nanosecond()), true},
		{"http-date past", now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"rfc850 date", now.Add(time.Hour).Format(time.RFC850), time.Hour, true},
		{"ansi c asctime", now.Add(30 * time.Second).Format(time.ANSIC), 30 * time.Second, true},
		{"garbage", "abc", 0, false},
		{"empty", "", 0, false},
		{"whitespace only", "   ", 0, false},
		{"surrounding ows", " 42 ", 42 * time.Second, true},
		{"float not allowed", "1.5", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseRetryAfter(tt.value, now)
			if ok != tt.wantOK {
				t.Fatalf("ParseRetryAfter(%q) ok = %v, want %v", tt.value, ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			// Tolerate sub-millisecond rounding from date formats.
			if diff := got - tt.want; diff < -time.Millisecond || diff > time.Millisecond {
				t.Errorf("ParseRetryAfter(%q) = %s, want ~%s", tt.value, got, tt.want)
			}
		})
	}
}

func TestApplyJitterDeterministic(t *testing.T) {
	// none is exact; unknown strategies degrade to none (deterministic).
	if got := applyJitter(3*time.Second, "none"); got != 3*time.Second {
		t.Errorf("jitter none = %s, want exact 3s", got)
	}
	if got := applyJitter(3*time.Second, "bogus"); got != 3*time.Second {
		t.Errorf("jitter bogus = %s, want exact 3s (deterministic fallback)", got)
	}
	if got := applyJitter(0, "full"); got != 0 {
		t.Errorf("jitter full on zero = %s, want 0", got)
	}
}

// TestComputeWaitJitterStatistics uses a seeded source to assert the
// distribution shape over many samples (guard against constant-folding the
// randomness away).
func TestComputeWaitJitterStatistics(t *testing.T) {
	sp := ScopePolicy{Backoff: "constant", Initial: 4 * time.Second, Max: 4 * time.Second, Jitter: "full", RetryAfter: "ignore"}
	_ = rand.Int64() // keep the v2 import exercised for seeded-source parity
	var total time.Duration
	const n = 1000
	for i := 0; i < n; i++ {
		total += ComputeWait(sp, 1, "", time.Hour, time.Now())
	}
	mean := time.Duration(total / n)
	// Mean of rand(0, 4s) should be near 2s; allow generous slack.
	if mean < 1500*time.Millisecond || mean > 2500*time.Millisecond {
		t.Errorf("full jitter mean = %s, want ~2s", mean)
	}
}

// TestComputeWaitRetryAfterNotJittered pins the audit semantics: an honored
// Retry-After REPLACES the computed backoff and is respected exactly — the
// server said how long to wait, so full jitter must not shorten it into
// rand(0, delay). Jitter applies only to the locally computed backoff.
func TestComputeWaitRetryAfterNotJittered(t *testing.T) {
	sp := ScopePolicy{
		Attempts:   10,
		Backoff:    "exponential",
		Initial:    1 * time.Second,
		Max:        8 * time.Second,
		Jitter:     "full", // default jitter; must NOT touch the honored value
		RetryAfter: "honor",
	}
	for i := 0; i < 50; i++ {
		if got := ComputeWait(sp, 1, "5", time.Hour, time.Now()); got != 5*time.Second {
			t.Fatalf("honored Retry-After: 5 must yield exactly 5s under any jitter, got %s", got)
		}
	}
	// Jitter still applies when there is no Retry-After (falls back to computed).
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[ComputeWait(sp, 1, "", time.Hour, time.Now())] = true
	}
	if len(seen) < 5 {
		t.Errorf("computed backoff without Retry-After should still be jittered, saw %d distinct values", len(seen))
	}
}

// TestComputeWaitOverflowSaturation pins the overflow guards: absurdly large
// attempts or initials must saturate at max, never wrap into a tiny wait.
func TestComputeWaitOverflowSaturation(t *testing.T) {
	sp := ScopePolicy{
		Attempts:   1000000,
		Backoff:    "exponential",
		Initial:    1 * time.Second,
		Max:        8 * time.Second,
		Jitter:     "none",
		RetryAfter: "ignore",
	}
	// attempt 1 is the first retry: 1s x 2^0 = 1s (not saturated yet);
	// by attempt 4 the curve reaches 8s and stays there.
	if got := ComputeWait(sp, 1, "", time.Hour, time.Now()); got != 1*time.Second {
		t.Fatalf("exponential attempt 1 = %s, want 1s", got)
	}
	for n := 4; n <= 200; n++ {
		if got := ComputeWait(sp, n, "", time.Hour, time.Now()); got != 8*time.Second {
			t.Fatalf("exponential attempt %d = %s, want 8s (saturated)", n, got)
		}
	}
	lin := sp
	lin.Backoff = "linear"
	lin.Initial = 1 << 62 // huge initial; x n must cap at max, not wrap
	for n := 2; n <= 5; n++ {
		if got := ComputeWait(lin, n, "", time.Hour, time.Now()); got != lin.Max {
			t.Fatalf("linear huge-initial attempt %d = %s, want max %s", n, got, lin.Max)
		}
	}
	exp := sp
	exp.Initial = 1 << 62
	for n := 2; n <= 5; n++ {
		if got := ComputeWait(exp, n, "", time.Hour, time.Now()); got != exp.Max {
			t.Fatalf("exponential huge-initial attempt %d = %s, want max %s", n, got, exp.Max)
		}
	}
}
