package reproxy

// header.go implements the retry control-plane policy grammar and its two
// carriers: the request header X-Reproxy-Retry-Policy and the leading policy
// segment of the proxy path (/+POLICY/https/host). Both carriers speak ONE
// grammar — semicolon-separated key=value pairs, whitespace-trimmed — fed
// through a shared pure transform into the url.Values shape policy.Parse
// consumes, so the two carriers get Parse's whole validation matrix and error
// bodies with zero new field rules.
//
// The only difference between the carriers is the spelling of status-code
// scopes: the header writes [429].attempts (brackets are legal in header
// values), the path segment writes 429.attempts (brackets are gen-delims,
// illegal in path segments). The dotted↔bracketed mapping is a total
// bijection on those scopes, locked by tests. The GLOBAL scope has no
// spelling of its own in either carrier: a bare field IS global
// (attempts=3), the [*]/*. spellings are dead.
//
// This file parses and strips only. Pipeline wiring — carrier resolution,
// the segment×header mutual exclusion, the single-attempt literal for the
// no-policy path — belongs to proxy.go.

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// RetryPolicyHeader is the single recognized member of the reserved
// X-Reproxy-* request-header namespace. Its value is a semicolon-separated
// list of key=value pairs using the policy pair grammar:
//
//	X-Reproxy-Retry-Policy: status=5xx; network=1; budget=30s; attempts=3; [429].attempts=5
const RetryPolicyHeader = "X-Reproxy-Retry-Policy"

// reproxyHeaderPrefix marks the reserved request-header namespace for
// reproxy<->client protocol. Unknown members are a 400 on every request in
// every mode (fail closed), and every member is stripped before the request
// is forwarded upstream: the upstream never sees the proxy's control plane.
const reproxyHeaderPrefix = "X-Reproxy-"

// carrierName labels a policy carrier in error messages.
type carrierName string

const (
	carrierHeader  carrierName = "X-Reproxy-Retry-Policy header"
	carrierSegment carrierName = "leading policy segment"
)

// policyCarrier is the per-carrier configuration of the shared pair grammar:
// how the carrier is named in error bodies and how one of its keys maps to
// the retry.-prefixed query spelling policy.Parse expects.
type policyCarrier struct {
	name carrierName
	// queryKeyFor maps one grammar key to its retry.-prefixed spelling.
	// For the header carrier the mapping is total (unknown keys pass
	// through unvalidated here and die inside Parse, preserving the
	// header channel's exact error bodies); for the segment carrier the
	// mapper validates key shape eagerly so bare words are a 400 at the
	// pair layer (see the bare-word rule below).
	queryKeyFor func(key string) (string, *RequestError)
}

// ParseRetryPolicyHeader parses X-Reproxy-Retry-Policy into the url.Values
// shape policy.Parse consumes, so the header channel gets Parse's whole
// validation matrix and error bodies with zero new rules.
//
// Grammar: pairs separated by ";", optional whitespace around pairs, each
// pair "key=value" with the key spelled exactly as a policy gate (status,
// network, budget), a bare scope field (FIELD — the global scope, e.g.
// attempts=3), or a bracketed status-code scope ([NNN].FIELD). Keys are
// case-sensitive; values are taken literally (never URL-decoded —
// header values are plain text). No legitimate value needs ";" or "=", so
// the pair split stays unambiguous (asserted by
// TestHeaderLegitimateValuesRepresentable).
//
// Absent header: returns (nil, nil) — the ONLY "no policy" spelling. A
// present-but-empty, whitespace-only, or empty-pair (";;") value is a 400
// (the normalization ladder): degenerate input must never read as "default
// policy", because Parse(url.Values{}) silently returns defaults
// (3 attempts, network=1, 30s budget).
//
// Also fail-closed on shape errors: multiple header occurrences, a pair
// without "=", an empty key, a value containing "=" (a second "=" can only
// be a paste error — the first "=" already ended the key), and any unknown
// X-Reproxy-* namespace member (checked first, so the namespace stays
// reserved wherever the policy is parsed).
func ParseRetryPolicyHeader(h http.Header) (url.Values, *RequestError) {
	if rerr := validateReproxyNamespace(h); rerr != nil {
		return nil, rerr
	}

	// Collect the policy header's values across every spelling in the map
	// (network-parsed headers are canonical; hand-built maps may not be).
	// One occurrence only, however it is spelled.
	var values []string
	for name, vals := range h {
		if http.CanonicalHeaderKey(name) == RetryPolicyHeader {
			values = append(values, vals...)
		}
	}
	switch len(values) {
	case 0:
		return nil, nil // absent: the only no-policy spelling
	case 1:
		return parsePolicyPairList(values[0], headerCarrier)
	default:
		return nil, &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("multiple %s headers: exactly one is allowed", RetryPolicyHeader),
			Hint:   "send a single X-Reproxy-Retry-Policy header and combine pairs with \";\"",
		}
	}
}

// The two carriers of the shared pair grammar. One grammar, two spellings of
// scope keys; everything else (separators, trimming, the fail-closed ladder,
// value handling) is identical by construction — both call
// parsePolicyPairList.
var headerCarrier = policyCarrier{
	name:        carrierHeader,
	queryKeyFor: queryKeyForHeaderKey,
}

var segmentCarrier = policyCarrier{
	name:        carrierSegment,
	queryKeyFor: queryKeyForSegmentKey,
}

// parsePolicyPairList parses one policy value into retry.-prefixed
// url.Values. This is the shared grammar both carriers speak: pairs split on
// ";", whitespace-trimmed, each pair "key=value", values never containing
// ";" or "=" (no legitimate policy value needs either, so the split is
// unambiguous by construction). Pair-level shape errors (empty pair, missing
// "=", empty key, stray "=") and — for carriers whose key mapper validates —
// unknown keys are 400s here; every field/value validation is deferred to
// policy.Parse (identical 400 matrix and error bodies for both carriers).
func parsePolicyPairList(value string, c policyCarrier) (url.Values, *RequestError) {
	bad := func(reason, hint string) *RequestError {
		return &RequestError{Code: 400, Reason: reason, Hint: hint}
	}

	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, bad(
			fmt.Sprintf("policy in the %s is present but empty: an absent policy is the only \"no policy\" spelling", c.name),
			"send pairs like \"status=5xx; attempts=3\" (a bare field IS global), or omit the policy entirely",
		)
	}

	params := url.Values{}
	for _, pair := range strings.Split(trimmed, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			return nil, bad(
				fmt.Sprintf("malformed policy in the %s: empty pair in value %q", c.name, value),
				"separate pairs with a single \";\" and no empty segments, e.g. \"status=5xx; network=1\"",
			)
		}
		key, val, found := strings.Cut(pair, "=")
		if !found {
			// The key is validated FIRST (the bare-word rule): any bare
			// word that is not a policy key is an unknown policy field,
			// the generic 400; only a key that IS a policy key missing
			// its "=" reaches this not-key=value branch.
			if _, kerr := c.queryKeyFor(strings.TrimSpace(pair)); kerr != nil {
				return nil, kerr
			}
			return nil, bad(
				fmt.Sprintf("malformed policy in the %s: pair %q is not key=value", c.name, pair),
				"write each pair as key=value, e.g. \"status=5xx; attempts=3\"",
			)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, bad(
				fmt.Sprintf("malformed policy in the %s: pair %q has an empty key", c.name, pair),
				"keys are status, network, budget, a bare scope field like attempts (global), or NNN.FIELD / [NNN].FIELD (a status code)",
			)
		}
		// Key shape: total for the header carrier (unknown keys die inside
		// Parse with Parse's exact error body), validated here for the
		// segment carrier (a bare word is an unknown policy field 400).
		queryKey, kerr := c.queryKeyFor(key)
		if kerr != nil {
			return nil, kerr
		}
		if strings.Contains(val, "=") {
			return nil, bad(
				fmt.Sprintf("malformed policy in the %s: value of pair %q contains \"=\": no retry value needs it", c.name, pair),
				"check the pair for a stray character; values never contain \"=\" or \";\"",
			)
		}
		// Pure transform, zero new key validation: the key is mapped to its
		// retry.-prefixed spelling and policy.Parse applies the whole
		// validation matrix — identical 400s for free.
		params.Add(queryKey, strings.TrimSpace(val))
	}
	return params, nil
}

// queryKeyForHeaderKey maps a header-carrier key to the retry.-prefixed
// spelling Parse expects. Gates gain the "retry." prefix; a BARE scope
// field gains "retry[*]." (a bare field IS the global scope); bracketed
// scope keys (which begin with "[") gain only "retry": the spelling is
// "retry[429].attempts", never "retry.[429].attempts". Keys that are
// neither a gate, a bare scope field, nor a bracketed scope pass through
// so Parse reports them with its own error body (preserving the header
// channel's exact 400s for unknown fields and bad values).
func queryKeyForHeaderKey(key string) (string, *RequestError) {
	if strings.HasPrefix(key, "[") {
		if !strings.HasPrefix(key, "[*]") {
			return "retry" + key, nil
		}
		return "", &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("unknown policy field %q", key),
			Hint:   "the global scope has no spelling of its own: a bare field IS global, e.g. \"attempts=3\"; brackets name a status code, e.g. \"[429].attempts=5\"",
		}
	}
	if scopeFields[key] {
		return "retry[*]." + key, nil
	}
	return "retry." + key, nil
}

// queryKeyForSegmentKey maps a leading-policy-segment key to the
// retry.-prefixed spelling Parse expects. The segment cannot carry brackets
// (gen-delims, illegal in path segments), so a status code scope is dotted:
//
//	NNN.FIELD -> retry[NNN].FIELD  (NNN a 3-digit status code)
//	gate      -> retry.gate        (status, network, budget)
//	FIELD     -> retry[*].FIELD    (a bare scope field IS the global scope;
//	                               the "*" spelling is dead)
//
// The gate set {status, network, budget} and the scope-field set
// {attempts, backoff, initial, max, jitter, retry_after} are disjoint, so a
// bare key's mapping is unambiguous.
//
// Unlike the header mapper this one validates key shape EAGERLY: the pair
// grammar has no place to defer to Parse for key spelling (the segment is
// parsed before any url.Values exist), and the bare-word rule requires bare
// words — "retry", "pure", "foo" — to die as the generic unknown-field 400.
// A malformed dotted scope that is not digits, or the dead "*.FIELD"
// global spelling, is reported as an unknown policy field too (the field
// set is closed; there is no other legal interpretation of a dotted key).
func queryKeyForSegmentKey(key string) (string, *RequestError) {
	if gateKeys[key] {
		return "retry." + key, nil
	}
	if scopeFields[key] {
		return "retry[*]." + key, nil
	}
	// Scope spelling: NNN.FIELD with NNN a 3-digit code.
	scope, field, found := strings.Cut(key, ".")
	if !found || field == "" || scope == "" || scope == "*" {
		return "", &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("unknown policy field %q", key),
			Hint:   "policy keys are status, network, budget, FIELD (a global scope field like attempts), or NNN.FIELD (a 3-digit status code), e.g. \"status=5xx; attempts=3; 429.attempts=5\"",
		}
	}
	if len(scope) != 3 || !isDigits(scope) {
		return "", &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("unknown policy field %q", key),
			Hint:   "policy keys are status, network, budget, FIELD (a global scope field like attempts), or NNN.FIELD (a 3-digit status code), e.g. \"status=5xx; attempts=3; 429.attempts=5\"",
		}
	}
	return "retry[" + scope + "]." + field, nil
}

// validateReproxyNamespace enforces the reserved X-Reproxy-* request-header
// namespace: any member other than RetryPolicyHeader is a 400 on every
// request in every mode — fail closed. ParseRetryPolicyHeader runs it;
// callers that take their policy from the leading segment must run it
// separately so the namespace stays reserved everywhere.
func validateReproxyNamespace(h http.Header) *RequestError {
	for name := range h {
		canonical := http.CanonicalHeaderKey(name)
		if !strings.HasPrefix(canonical, reproxyHeaderPrefix) {
			continue
		}
		if canonical != RetryPolicyHeader {
			return &RequestError{
				Code:   400,
				Reason: fmt.Sprintf("unknown header %q: the %s namespace is reserved", canonical, reproxyHeaderPrefix),
				Hint:   fmt.Sprintf("the only recognized %s header is %s; remove the header or rename it outside the reserved namespace", reproxyHeaderPrefix, RetryPolicyHeader),
			}
		}
	}
	return nil
}

// isReproxyHeader reports whether name belongs to the reserved X-Reproxy-*
// namespace. Used to strip the proxy's control-plane headers before
// forwarding upstream, alongside hop-by-hop stripping (buildOutboundHeaders).
func isReproxyHeader(name string) bool {
	return strings.HasPrefix(http.CanonicalHeaderKey(name), reproxyHeaderPrefix)
}
