package reproxy

import (
	"fmt"
	"net/url"
	"strings"
)

// retryNamespacePrefix marks query keys reserved for the proxy's retry
// protocol. Keys starting with "retry." or "retry[" are stripped from the
// passthrough query and routed to retryParams; anything else (retryfoo,
// myretry.x) is client data and passes through untouched.
const retryNamespacePrefix = "retry"

// gateKeys are the retry keys that configure retry conditions (gates) rather
// than retry shaping (scope fields).
var gateKeys = map[string]bool{
	"status":  true,
	"network": true,
	"budget":  true,
}

// scopeFields are the retry policy fields recognized inside retry[*].FIELD
// and retry[NNN].FIELD scopes. FIELD itself is not validated here; policy
// parsing owns that (fail closed with a 400 naming the key).
//
// SplitQuery only needs to decide namespace membership, but it recognizes
// these shapes so unknown keys produce a precise error naming the offending
// key instead of a generic one.
var scopeFields = map[string]bool{
	"attempts":    true,
	"backoff":     true,
	"initial":     true,
	"max":         true,
	"jitter":      true,
	"retry_after": true,
}

// SplitQuery splits a raw query string into the upstream passthrough portion
// and the retry-namespace parameters.
//
// The passthrough portion is rebuilt from the original raw segments so that
// byte order and encoding are preserved exactly: no parsing or
// re-serialization happens (signed-URL safety). Valueless keys ("?a&b"),
// empty values ("a="), and duplicate keys all round-trip verbatim.
//
// Retry-namespace keys are collected into retryParams with URL-decoded
// values (they are re-validated later by policy parsing, which fails closed
// with a 400 naming the offending key). Unrecognized retry.* keys are a 400.
func SplitQuery(rawQuery string) (upstreamQuery string, retryParams url.Values, err *RequestError) {
	if rawQuery == "" {
		return "", url.Values{}, nil
	}

	// Limit on encoded bytes per key to bound retryParams memory.
	const maxKeyLen = 256

	var passthrough []string
	retryParams = url.Values{}

	for _, segment := range strings.Split(rawQuery, "&") {
		key := segment
		value := ""
		if i := strings.Index(segment, "="); i >= 0 {
			key, value = segment[:i], segment[i+1:]
		}

		if !inRetryNamespace(key) {
			passthrough = append(passthrough, segment)
			continue
		}

		name, rerr := validateRetryKey(key, maxKeyLen)
		if rerr != nil {
			return "", nil, rerr
		}
		decodedValue := value
		if d, derr := url.QueryUnescape(value); derr == nil {
			decodedValue = d
		}
		retryParams.Add(name, decodedValue)
	}

	return strings.Join(passthrough, "&"), retryParams, nil
}

// hasRetryKeys reports whether rawQuery carries ANY retry-namespace key
// (retry.* / retry[...]). It is a light, inert scan for the deprecation gate
// (design §5): unlike SplitQuery it never validates, never 400s, and never
// decodes — unknown or malformed retry keys still count as "present" (the
// request spelled the namespace; the later SplitQuery in retry mode will
// produce the precise 400 if the key is actually invalid).
func hasRetryKeys(rawQuery string) bool {
	if rawQuery == "" {
		return false
	}
	for _, segment := range strings.Split(rawQuery, "&") {
		key := segment
		if i := strings.Index(segment, "="); i >= 0 {
			key = segment[:i]
		}
		if inRetryNamespace(key) {
			return true
		}
	}
	return false
}

// inRetryNamespace reports whether a raw key belongs to the retry protocol:
// it starts with "retry." or "retry[" exactly (retryfoo/myretry.x do not).
func inRetryNamespace(key string) bool {
	if key == retryNamespacePrefix {
		return false
	}
	return strings.HasPrefix(key, "retry.") || strings.HasPrefix(key, "retry[")
}

// validateRetryKey checks the shape of a retry-namespace key and returns its
// canonical name for retryParams. Recognized shapes:
//
//	retry.status, retry.network, retry.budget   (gates)
//	retry[*].FIELD                              (default scope)
//	retry[NNN].FIELD                            (exact status scope, NNN 100-599)
//
// Anything else in the namespace is a 400 naming the key. Note: NNN range
// validation (100-599) is policy's job per the module contract; here the
// shape must merely be digits, but since "digits" is cheap we also reject
// non-digits with a 400 (a retry[abc].x key can never be valid).
func validateRetryKey(key string, maxKeyLen int) (string, *RequestError) {
	if len(key) > maxKeyLen {
		return "", &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("unknown retry parameter %q: key too long", key),
			Hint:   "retry parameters follow the form retry.status, retry.network, retry.budget, retry[*].field, or retry[NNN].field",
		}
	}

	// Gate keys: retry.status / retry.network / retry.budget.
	if name, ok := strings.CutPrefix(key, "retry."); ok {
		if gateKeys[name] {
			return "retry." + name, nil
		}
		return "", &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("unknown retry parameter %q", key),
			Hint:   "recognized keys: retry.status, retry.network, retry.budget, retry[*].FIELD, retry[NNN].FIELD",
		}
	}

	// Scope keys: retry[SCOPE].FIELD.
	rest, ok := strings.CutPrefix(key, "retry[")
	if !ok {
		return "", &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("malformed retry parameter %q", key),
			Hint:   "retry parameters follow the form retry.status, retry.network, retry.budget, retry[*].field, or retry[NNN].field",
		}
	}
	scopeEnd := strings.Index(rest, "]")
	if scopeEnd < 0 {
		return "", &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("malformed retry parameter %q: missing \"]\"", key),
			Hint:   "scope keys look like retry[*].attempts or retry[429].attempts",
		}
	}
	scope := rest[:scopeEnd]
	tail := rest[scopeEnd+1:]

	field, ok := strings.CutPrefix(tail, ".")
	if !ok || field == "" {
		return "", &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("malformed retry parameter %q: missing field after the scope", key),
			Hint:   "scope keys look like retry[*].attempts or retry[429].attempts",
		}
	}

	// Validate the scope first: a malformed scope is the more fundamental
	// error and produces a clearer diagnosis than an unknown field would.
	switch {
	case scope != "*" && !(isDigits(scope) && len(scope) == 3):
		return "", &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("invalid retry scope %q in %q: must be \"*\" or a 3-digit status code", scope, key),
			Hint:   "scopes are retry[*] for defaults or retry[NNN] with an exact 3-digit status code, e.g. retry[429].attempts",
		}
	case !scopeFields[field]:
		return "", &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("unknown retry parameter %q", key),
			Hint:   "recognized fields inside a scope: attempts, backoff, initial, max, jitter, retry_after",
		}
	}
	return "retry[" + scope + "]." + field, nil
}

// isDigits reports whether s is non-empty and all decimal digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
