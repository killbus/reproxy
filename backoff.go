package reproxy

import (
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// ParseRetryAfter parses a Retry-After header value: either
// delay-seconds (an integer, per RFC 9110 section 10.2.3) or an HTTP-date
// (all three formats http.ParseTime covers). A date in the past means zero
// delay. The bool result reports whether the value parsed at all; callers
// fall back to their computed backoff otherwise.
func ParseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = trimOWS(v)
	if v == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			n = 0
		}
		return time.Duration(n) * time.Second, true
	}
	if ts, err := http.ParseTime(v); err == nil {
		d := ts.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// ComputeWait returns how long to wait before retry attempt number
// `attempt` (1-based: the first retry after the initial request's failure).
//
// The formula set is pinned by the audit spec:
//
//	exponential: wait(n) = min(initial x 2^(n-1), max)
//	linear:      wait(n) = min(initial x n, max)
//	constant:    wait    = initial
//
// Jitter then applies: none keeps the computed value; full returns
// rand(0, computed); equal returns computed/2 + rand(0, computed/2).
//
// When sp.RetryAfter is "honor" and retryAfterHeader parses, the parsed
// delay REPLACES the computed backoff entirely (the server said how long
// to wait, so it is respected exactly), though never above sp.Max. A
// server-indicated wait is NOT jittered: "respect what the server said"
// means waiting the full delay, not rand(0, delay) (the audit pins
// replacement, and Envoy's rate_limited_retry_back_off / cenkalti's
// RetryAfter both use the value as-is). Jitter applies only to the
// locally computed backoff.
//
// The final wait is always capped to remainingBudget so a backoff can
// never exceed the request's total retry budget.
func ComputeWait(sp ScopePolicy, attempt int, retryAfterHeader string, remainingBudget time.Duration, now time.Time) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	computed := sp.Initial
	switch sp.Backoff {
	case "exponential":
		// wait(n) = min(initial x 2^(n-1), max). The shift can overflow
		// int64 for large attempts (wrapping negative, or wrapping so far
		// it lands back in the positive range and looks tiny); any
		// overflow means the curve is past max, so cap there.
		shift := uint(attempt - 1)
		if shift >= 62 || initialShiftOverflow(sp.Initial, shift) || sp.Initial<<shift > sp.Max {
			computed = sp.Max
		} else {
			computed = sp.Initial << shift
		}
	case "linear":
		// initial x attempt with overflow guard: a wrapped-negative (or
		// re-wrapped positive-below-max) product must cap at max, not
		// produce a tiny wait.
		if a, ok := mulDurOK(sp.Initial, time.Duration(attempt)); ok && a <= sp.Max {
			computed = a
		} else {
			computed = sp.Max
		}
	default: // "constant" (and any unknown value degrades to constant)
		computed = sp.Initial
	}

	wait := computed

	// Honor mode: a parseable Retry-After replaces the computed backoff,
	// capped to sp.Max (never wait longer than the policy's single-wait cap
	// — never hang on "Retry-After: 3600") and NOT jittered (see doc
	// comment). With no parseable Retry-After, the computed backoff is
	// jittered as usual.
	honored := false
	if sp.RetryAfter == "honor" {
		if d, ok := ParseRetryAfter(retryAfterHeader, now); ok {
			wait = d
			if wait > sp.Max {
				wait = sp.Max
			}
			honored = true
		}
	}
	if !honored {
		wait = applyJitter(wait, sp.Jitter)
	}

	// Budget cap: never wait past the remaining retry budget. A zero or
	// negative budget means the lifecycle is over: nothing left to wait on.
	if wait > remainingBudget {
		wait = remainingBudget
	}
	if wait < 0 {
		wait = 0
	}
	return wait
}

// applyJitter applies the jitter strategy to the computed wait:
// none keeps it, full randomizes into [0, computed), equal randomizes into
// [computed/2, computed).
func applyJitter(wait time.Duration, jitter string) time.Duration {
	switch jitter {
	case "full":
		if wait <= 0 {
			return 0
		}
		return time.Duration(rand.Int64N(int64(wait)))
	case "equal":
		if wait <= 0 {
			return 0
		}
		half := wait / 2
		return half + time.Duration(rand.Int64N(int64(half)))
	default: // "none" and unknown values: deterministic
		return wait
	}
}

// mulDurOK multiplies two durations, reporting whether the product fits in
// int64 without overflow. A false result means the true product exceeds the
// int64 range, so callers must treat it as "beyond any cap".
func mulDurOK(a, b time.Duration) (time.Duration, bool) {
	p := a * b
	if a != 0 && p/a != b {
		return 0, false // overflowed and wrapped
	}
	return p, true
}

// initialShiftOverflow reports whether initial << shift overflows int64
// (the true value no longer fits, even if the wrapped result looks positive
// and small). Callers guard shift >= 62 themselves; here 0 <= shift < 62:
// the shift is exact iff initial < 2^(63-shift), i.e. its top (63-shift)
// bits are zero.
func initialShiftOverflow(initial time.Duration, shift uint) bool {
	if initial <= 0 {
		return false // caller caps non-positive initial separately
	}
	return initial>>(63-shift) != 0
}

// trimOWS strips optional whitespace around a header value.
func trimOWS(v string) string {
	start := 0
	for start < len(v) && (v[start] == ' ' || v[start] == '\t') {
		start++
	}
	end := len(v)
	for end > start && (v[end-1] == ' ' || v[end-1] == '\t') {
		end--
	}
	return v[start:end]
}
