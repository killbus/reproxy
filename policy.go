package reproxy

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ScopePolicy is the field set governing how retries are shaped: how many
// attempts, which backoff curve, how the waits are jittered, and whether
// upstream Retry-After headers are honored. A ScopePolicy applies to a scope:
// the default scope (retry[*]) or an exact status code (retry[NNN]).
type ScopePolicy struct {
	// Attempts is the TOTAL attempt count including the first request
	// (attempts=1 means no retry). This matches gRPC/AWS/nginx precedent
	// and avoids off-by-one disputes.
	Attempts int
	// Backoff is one of "constant", "linear", "exponential".
	Backoff string
	// Initial is the base wait duration; Max caps a single computed wait.
	Initial, Max time.Duration
	// Jitter is one of "none", "full", "equal".
	Jitter string
	// RetryAfter is one of "honor", "ignore".
	RetryAfter string
}

// Defaults for ScopePolicy fields.
const (
	DefaultAttempts   = 3
	DefaultBackoff    = "exponential"
	DefaultInitial    = 1 * time.Second
	DefaultMaxWait    = 8 * time.Second
	DefaultJitter     = "full"
	DefaultRetryAfter = "honor"
	DefaultBudget     = 30 * time.Second
)

// Policy is a fully resolved retry policy for one request: the effective
// per-scope field sets, the retry gates (which failures trigger retries), and
// the total lifecycle budget.
type Policy struct {
	// Default is the effective default-scope policy (built-in defaults
	// overridden by retry[*] fields, then clamped by the server config).
	Default ScopePolicy
	// ByStatus holds one merged, ready-to-use ScopePolicy per exact status
	// code that has a retry[NNN] override. Every entry is a FULL ScopePolicy:
	// default fields overridden only where the scope specified them, then
	// clamped by the server config.
	ByStatus map[int]ScopePolicy
	// StatusGate is the sorted, deduplicated set of status codes that
	// trigger a retry (from retry.status). Empty means no status-based retry.
	StatusGate []int
	// NetworkGate reports whether network failures (dial/TLS/write/TTFB
	// timeout) are retried (from retry.network, default true).
	NetworkGate bool
	// Budget is the total retry lifecycle duration (from retry.budget,
	// clamped by the server's MaxBudget).
	Budget time.Duration
}

// Effective returns the ScopePolicy to use when a retry is triggered by the
// given upstream status code: the per-status override if present, else the
// default scope.
func (p Policy) Effective(status int) ScopePolicy {
	if sp, ok := p.ByStatus[status]; ok {
		return sp
	}
	return p.Default
}

// RetryableStatus reports whether the given status code is in the status gate.
func (p Policy) RetryableStatus(status int) bool {
	for _, s := range p.StatusGate {
		if s == status {
			return true
		}
	}
	return false
}

// Parse builds a Policy from the retry-namespace query parameters produced by
// SplitQuery.
//
// Resolution is a three-tier chain with field-level override:
//
//	built-in defaults -> retry[*].FIELD -> retry[NNN].FIELD
//
// followed by a server-config clamp (MaxAttempts, MaxBudget) that narrows and
// never widens. Gates (retry.status, retry.network, retry.budget) live only at
// the [*] level; per-status gates are a 400.
//
// Any invalid value fails closed with a 400 RequestError naming the offending
// key, including the dead-config case: a retry[NNN] scope whose code is not
// in retry.status.
func Parse(params url.Values, cfg *ServerConfig) (Policy, *RequestError) {
	base := ScopePolicy{
		Attempts:   DefaultAttempts,
		Backoff:    DefaultBackoff,
		Initial:    DefaultInitial,
		Max:        DefaultMaxWait,
		Jitter:     DefaultJitter,
		RetryAfter: DefaultRetryAfter,
	}

	policy := Policy{
		Default:     base,
		ByStatus:    map[int]ScopePolicy{},
		StatusGate:  nil,
		NetworkGate: true,
		Budget:      DefaultBudget,
	}

	// First pass: gates and the default scope, validating every key shape so
	// nothing passes through unvalidated (fail closed, mirroring SplitQuery).
	for key, vals := range params {
		if len(vals) == 0 {
			continue
		}
		value := vals[len(vals)-1] // duplicate key: last wins
		switch key {
		case "retry.status":
			codes, err := parseStatusGate(value)
			if err != nil {
				return Policy{}, err
			}
			policy.StatusGate = codes
		case "retry.network":
			gate, err := parseNetworkGate(value)
			if err != nil {
				return Policy{}, err
			}
			policy.NetworkGate = gate
		case "retry.budget":
			d, err := parseBudget(value)
			if err != nil {
				return Policy{}, err
			}
			policy.Budget = d
		default:
			if _, _, ok := splitScopeKey(key); ok {
				break // exact-status scopes are resolved in the second pass
			}
			field, ok := strings.CutPrefix(key, "retry[*].")
			if !ok {
				return Policy{}, &RequestError{
					Code:   400,
					Reason: fmt.Sprintf("unknown retry parameter %q", key),
					Hint:   "recognized keys: retry.status, retry.network, retry.budget, retry[*].FIELD, retry[NNN].FIELD (with NNN a status code from 100 to 599)",
				}
			}
			if err := applyScopeField(&policy.Default, field, key, value); err != nil {
				return Policy{}, err
			}
		}
	}

	// Second pass: exact-status scopes, merged over the default scope.
	for key, vals := range params {
		if len(vals) == 0 {
			continue
		}
		value := vals[len(vals)-1]
		scope, field, ok := splitScopeKey(key)
		if !ok {
			continue // gates and default scope were handled (and validated) above
		}
		sp, seen := policy.ByStatus[scope]
		if !seen {
			sp = policy.Default // start from the resolved default scope
		}
		if err := applyScopeField(&sp, field, key, value); err != nil {
			return Policy{}, err
		}
		policy.ByStatus[scope] = sp
	}

	// Cross-field sanity: max < initial never makes sense. Checked once per
	// resolved scope AFTER all fields are applied, so the verdict does not
	// depend on map iteration order (fields may arrive in any order).
	if err := validateScopeCrossFields(policy.Default, "retry[*]"); err != nil {
		return Policy{}, err
	}
	for scope, sp := range policy.ByStatus {
		if err := validateScopeCrossFields(sp, fmt.Sprintf("retry[%d]", scope)); err != nil {
			return Policy{}, err
		}
	}

	// Dead-config check: a retry[NNN] scope whose code never triggers a retry
	// (429 not in retry.status) is a 400, mirroring the unknown-key policy.
	for scope := range policy.ByStatus {
		if !policy.RetryableStatus(scope) {
			return Policy{}, &RequestError{
				Code:   400,
				Reason: fmt.Sprintf("dead retry configuration: retry[%d] overrides a status code that is not in retry.status", scope),
				Hint:   "add the code to retry.status (e.g. retry.status=429) or remove the retry[NNN] parameters",
			}
		}
	}

	// Server clamps: narrow, never widen.
	if cfg != nil {
		if cfg.MaxAttempts > 0 && policy.Default.Attempts > cfg.MaxAttempts {
			policy.Default.Attempts = cfg.MaxAttempts
		}
		for scope, sp := range policy.ByStatus {
			if cfg.MaxAttempts > 0 && sp.Attempts > cfg.MaxAttempts {
				sp.Attempts = cfg.MaxAttempts
				policy.ByStatus[scope] = sp
			}
		}
		if cfg.MaxBudget > 0 && policy.Budget > cfg.MaxBudget {
			policy.Budget = cfg.MaxBudget
		}
	}
	return policy, nil
}

// SingleAttemptPolicy returns the pure-mode, headerless policy: exactly one
// attempt, every retry gate off (design D13). It is a LITERAL — never
// Parse(url.Values{}), which returns the v0.1.0 defaults (3 attempts,
// network=1, 30s budget) and would smuggle a retry lifecycle into pure mode.
// The shape fields carry inert defaults: with one attempt no wait is ever
// computed, so they never influence behavior.
//
// Budget is the one field that stays live: it is not a retry-lifecycle cap
// here but the bound feeding the per-try TTFB timeout (which still applies in
// pure mode — it bounds a hang, not a retry). It defaults to DefaultBudget
// and is narrowed by the server's MaxBudget clamp, mirroring Parse.
func SingleAttemptPolicy(cfg *ServerConfig) Policy {
	budget := DefaultBudget
	if cfg != nil && cfg.MaxBudget > 0 && budget > cfg.MaxBudget {
		budget = cfg.MaxBudget
	}
	return Policy{
		Default: ScopePolicy{
			Attempts:   1,
			Backoff:    DefaultBackoff,
			Initial:    DefaultInitial,
			Max:        DefaultMaxWait,
			Jitter:     DefaultJitter,
			RetryAfter: DefaultRetryAfter,
		},
		ByStatus:    map[int]ScopePolicy{},
		StatusGate:  nil,
		NetworkGate: false,
		Budget:      budget,
	}
}

// applyScopeField parses one FIELD=value and records it on sp. The key is
// carried through to error messages so the client sees which key failed.
// Unknown fields (including gate names like status/network/budget, which are
// global-only) are a 400: gates are not per-status shapable.
func applyScopeField(sp *ScopePolicy, field, key, value string) *RequestError {
	if !scopeFields[field] {
		return &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("unknown retry parameter %q", key),
			Hint:   "recognized fields inside a scope: attempts, backoff, initial, max, jitter, retry_after; gates (status, network, budget) are set globally as retry.status / retry.network / retry.budget",
		}
	}
	switch field {
	case "attempts":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return &RequestError{
				Code:   400,
				Reason: fmt.Sprintf("invalid value %q for %q: attempts must be an integer >= 1 (total attempts including the first; 1 means no retry)", value, key),
				Hint:   "example: retry[*].attempts=3",
			}
		}
		sp.Attempts = n
	case "backoff":
		switch value {
		case "constant", "linear", "exponential":
			sp.Backoff = value
		default:
			return &RequestError{
				Code:   400,
				Reason: fmt.Sprintf("invalid value %q for %q: backoff must be constant, linear, or exponential", value, key),
				Hint:   "example: retry[*].backoff=linear",
			}
		}
	case "initial":
		d, err := parseDurationField(value)
		if err != nil {
			return &RequestError{
				Code:   400,
				Reason: fmt.Sprintf("invalid value %q for %q: %s", value, key, err),
				Hint:   "durations require a unit, e.g. retry[*].initial=1s or retry[*].initial=100ms",
			}
		}
		sp.Initial = d
	case "max":
		d, err := parseDurationField(value)
		if err != nil {
			return &RequestError{
				Code:   400,
				Reason: fmt.Sprintf("invalid value %q for %q: %s", value, key, err),
				Hint:   "durations require a unit, e.g. retry[*].max=8s",
			}
		}
		sp.Max = d
	case "jitter":
		switch value {
		case "none", "full", "equal":
			sp.Jitter = value
		default:
			return &RequestError{
				Code:   400,
				Reason: fmt.Sprintf("invalid value %q for %q: jitter must be none, full, or equal", value, key),
				Hint:   "example: retry[*].jitter=equal",
			}
		}
	case "retry_after":
		switch value {
		case "honor", "ignore":
			sp.RetryAfter = value
		default:
			return &RequestError{
				Code:   400,
				Reason: fmt.Sprintf("invalid value %q for %q: retry_after must be honor or ignore", value, key),
				Hint:   "example: retry[*].retry_after=ignore",
			}
		}
	}
	// Cross-field sanity is deliberately NOT checked here: it runs after
	// the whole scope is assembled (validateScopeCrossFields), so the
	// verdict cannot depend on the order the map yields keys in.
	return nil
}

// validateScopeCrossFields rejects a resolved scope where max < initial.
// It runs after the scope is fully assembled (see Parse), so intermediate
// states during field application never trigger it.
func validateScopeCrossFields(sp ScopePolicy, scopeName string) *RequestError {
	if sp.Max < sp.Initial {
		return &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("invalid value for %q: max (%s) is smaller than initial (%s)", scopeName+".max", sp.Max, sp.Initial),
			Hint:   fmt.Sprintf("raise %s.max or lower %s.initial; max must be >= initial", scopeName, scopeName),
		}
	}
	return nil
}

// splitScopeKey decomposes a "retry[NNN].FIELD" key. Gate keys and the
// default-scope form are reported as not-an-exact-status-scope.
func splitScopeKey(key string) (scope int, field string, ok bool) {
	rest, found := strings.CutPrefix(key, "retry[")
	if !found {
		return 0, "", false
	}
	end := strings.Index(rest, "]")
	if end < 0 {
		return 0, "", false
	}
	scopeStr := rest[:end]
	if len(scopeStr) != 3 || !isDigits(scopeStr) {
		return 0, "", false
	}
	field = rest[end+1:]
	if !strings.HasPrefix(field, ".") {
		return 0, "", false
	}
	scope, err := strconv.Atoi(scopeStr)
	if err != nil || scope < 100 || scope > 599 {
		return 0, "", false
	}
	return scope, field[1:], true
}

// parseStatusGate parses the retry.status value: a comma-separated list of
// exact 3-digit codes (100-599), closed ranges ("500-599"), and class
// shorthands ("4xx"/"5XX", case-insensitive). Overlaps and duplicates
// silently union and dedupe. Whitespace, reversed or open ranges, empty
// items, and out-of-range codes are 400s.
func parseStatusGate(value string) ([]int, *RequestError) {
	bad := func(reason, hint string) *RequestError {
		return &RequestError{Code: 400, Reason: reason, Hint: hint}
	}
	if value == "" {
		return nil, bad(
			"invalid value \"\" for \"retry.status\": must list at least one status code",
			"example: retry.status=429,500-599 or retry.status=5xx",
		)
	}
	set := map[int]bool{}
	for _, item := range strings.Split(value, ",") {
		if item == "" {
			return nil, bad(
				fmt.Sprintf("invalid value %q for \"retry.status\": empty item in list", value),
				"separate items with a bare comma, e.g. retry.status=429,500",
			)
		}
		lower := strings.ToLower(item)
		// Class shorthand: Nxx (only 4xx/5xx make sense as retry codes).
		if strings.HasSuffix(lower, "xx") && len(lower) == 3 && lower[0] >= '1' && lower[0] <= '5' && isDigits(string(lower[0])) {
			class := int(lower[0]-'0') * 100
			for c := class; c < class+100; c++ {
				if c >= 100 {
					set[c] = true
				}
			}
			continue
		}
		// Range: closed, low-high, both 3-digit codes in 100-599.
		if lo, hi, found := strings.Cut(item, "-"); found {
			l, lerr := parseStatusCode(lo)
			if lerr != nil {
				return nil, lerr
			}
			h, herr := parseStatusCode(hi)
			if herr != nil {
				return nil, herr
			}
			if l > h {
				return nil, bad(
					fmt.Sprintf("invalid range %q in \"retry.status\": bounds are reversed", item),
					"write ranges low-to-high, e.g. retry.status=500-599",
				)
			}
			for c := l; c <= h; c++ {
				set[c] = true
			}
			continue
		}
		code, err := parseStatusCode(item)
		if err != nil {
			return nil, err
		}
		set[code] = true
	}
	if len(set) == 0 {
		return nil, bad(
			fmt.Sprintf("invalid value %q for \"retry.status\": no valid status codes", value),
			"example: retry.status=429,500-599 or retry.status=5xx",
		)
	}
	codes := make([]int, 0, len(set))
	for c := range set {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	return codes, nil
}

// parseStatusCode parses one exact status: exactly 3 digits, value 100-599.
func parseStatusCode(s string) (int, *RequestError) {
	if len(s) != 3 || !isDigits(s) {
		return 0, &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("invalid status code %q in \"retry.status\": must be a 3-digit code from 100 to 599, a closed range like 500-599, or a class like 5xx", s),
			Hint:   "example: retry.status=429,500-599 or retry.status=5xx",
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 100 || n > 599 {
		return 0, &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("status code %q out of range in \"retry.status\": must be 100-599", s),
			Hint:   "status codes run from 100 to 599",
		}
	}
	return n, nil
}

// parseNetworkGate parses retry.network: only "0" and "1" are accepted (a
// valueless or empty key is a client error — fail closed on invalid values,
// matching the documented contract).
func parseNetworkGate(value string) (bool, *RequestError) {
	switch value {
	case "1":
		return true, nil
	case "0":
		return false, nil
	default:
		return false, &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("invalid value %q for \"retry.network\": must be 0 or 1", value),
			Hint:   "retry.network=1 retries network failures (default), retry.network=0 disables them",
		}
	}
}

// parseBudget parses retry.budget: a duration with a mandatory unit.
func parseBudget(value string) (time.Duration, *RequestError) {
	d, err := parseDurationField(value)
	if err != nil {
		return 0, &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("invalid value %q for \"retry.budget\": %s", value, err),
			Hint:   "durations require a unit, e.g. retry.budget=30s or retry.budget=90s",
		}
	}
	if d <= 0 {
		return 0, &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("invalid value %q for \"retry.budget\": must be > 0", value),
			Hint:   "example: retry.budget=30s",
		}
	}
	return d, nil
}

// parseDurationField wraps time.ParseDuration so callers get a stable error
// string (bare numbers are rejected by ParseDuration itself, which requires
// a unit — exactly the behavior the spec pins).
func parseDurationField(value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("must be a duration with a unit (got %q)", value)
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be > 0 (got %q)", value)
	}
	return d, nil
}
