package reproxy

// header.go implements the header channel of the retry control plane
// (design §2): a reserved X-Reproxy-* namespace for reproxy<->client
// protocol whose single member, X-Reproxy-Retry-Policy, carries a retry
// policy out-of-band so the request query can stay target-owned.
//
// This file parses and strips only. Pipeline wiring — mode resolution, the
// query/header mutual exclusion, the single-attempt literal for headerless
// pure mode — belongs to proxy.go (Batch 3).

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// RetryPolicyHeader is the single recognized member of the reserved
// X-Reproxy-* request-header namespace. Its value is a semicolon-separated
// list of key=value pairs using the query channel's key grammar minus the
// "retry." prefix:
//
//	X-Reproxy-Retry-Policy: status=5xx; network=1; budget=30s; [*].attempts=3; [429].attempts=5
const RetryPolicyHeader = "X-Reproxy-Retry-Policy"

// reproxyHeaderPrefix marks the reserved request-header namespace for
// reproxy<->client protocol. Unknown members are a 400 on every request in
// every mode (fail closed, mirroring unknown retry.* query keys), and every
// member is stripped before the request is forwarded upstream: the upstream
// never sees the proxy's control plane.
const reproxyHeaderPrefix = "X-Reproxy-"

// ParseRetryPolicyHeader parses X-Reproxy-Retry-Policy into the url.Values
// shape policy.Parse consumes — the same shape SplitQuery produces for
// retry-namespace query keys — so the header channel gets Parse's whole
// validation matrix and error bodies with zero new rules.
//
// Grammar: pairs separated by ";", optional whitespace around pairs, each
// pair "key=value" with the key spelled exactly as the query channel's
// post-"retry." remainder: gates (status, network, budget) and scope fields
// ([*].FIELD, [NNN].FIELD). Keys are case-sensitive; values are taken
// literally (never URL-decoded — header values are plain text, unlike query
// components). No legitimate value needs ";" or "=", so the pair split stays
// unambiguous (asserted by TestHeaderLegitimateValuesRepresentable).
//
// Absent header: returns (nil, nil) — the ONLY "no policy" spelling. A
// present-but-empty, whitespace-only, or empty-pair (";;") value is a 400
// (the normalization ladder): degenerate input must never read as "default
// policy", because Parse(url.Values{}) silently returns the v0.1.0 defaults
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
		return parseRetryPolicyHeaderValue(values[0])
	default:
		return nil, &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("multiple %s headers: exactly one is allowed", RetryPolicyHeader),
			Hint:   "send a single X-Reproxy-Retry-Policy header and combine pairs with \";\"",
		}
	}
}

// parseRetryPolicyHeaderValue parses one X-Reproxy-Retry-Policy value into
// retry.-prefixed url.Values. Pair-level shape errors (empty pair, missing
// "=", empty key, stray "=") are 400s here; every field/value validation is
// deferred to policy.Parse (identical 400 matrix and error bodies).
func parseRetryPolicyHeaderValue(value string) (url.Values, *RequestError) {
	bad := func(reason, hint string) *RequestError {
		return &RequestError{Code: 400, Reason: reason, Hint: hint}
	}

	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, bad(
			fmt.Sprintf("%s header is present but empty: an absent header is the only \"no policy\" spelling", RetryPolicyHeader),
			"send pairs like \"status=5xx; [*].attempts=3\", or omit the header entirely",
		)
	}

	params := url.Values{}
	for _, pair := range strings.Split(trimmed, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			return nil, bad(
				fmt.Sprintf("malformed %s header: empty pair in value %q", RetryPolicyHeader, value),
				"separate pairs with a single \";\" and no empty segments, e.g. \"status=5xx; network=1\"",
			)
		}
		key, val, found := strings.Cut(pair, "=")
		if !found {
			return nil, bad(
				fmt.Sprintf("malformed %s header: pair %q is not key=value", RetryPolicyHeader, pair),
				"write each pair as key=value, e.g. \"status=5xx; [*].attempts=3\"",
			)
		}
		if key == "" {
			return nil, bad(
				fmt.Sprintf("malformed %s header: pair %q has an empty key", RetryPolicyHeader, pair),
				"keys are status, network, budget, [*].FIELD, or [NNN].FIELD",
			)
		}
		if strings.Contains(val, "=") {
			return nil, bad(
				fmt.Sprintf("malformed %s header: value of pair %q contains \"=\": no retry value needs it", RetryPolicyHeader, pair),
				"check the pair for a stray character; values never contain \"=\" or \";\"",
			)
		}
		// Pure transform, zero new key validation (design §2): the key is
		// prefixed to its query spelling and policy.Parse applies the whole
		// query-channel validation matrix — identical 400s for free.
		params.Add(queryKeyForHeaderKey(key), val)
	}
	return params, nil
}

// queryKeyForHeaderKey maps a header-channel key to the query-channel key
// Parse expects. Gates gain the "retry." prefix; scope keys (which begin
// with "[") gain only "retry": the query channel's spelling is
// "retry[*].attempts", never "retry.[*].attempts".
func queryKeyForHeaderKey(key string) string {
	if strings.HasPrefix(key, "[") {
		return "retry" + key
	}
	return "retry." + key
}

// validateReproxyNamespace enforces the reserved X-Reproxy-* request-header
// namespace (design §2): any member other than RetryPolicyHeader is a 400 on
// every request in every mode — fail closed, mirroring unknown retry.*
// query keys. ParseRetryPolicyHeader runs it; modes that do not parse the
// policy header (the query channel) must run it separately so the namespace
// stays reserved everywhere.
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
