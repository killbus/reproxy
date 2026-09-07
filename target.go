package reproxy

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// PathTarget is an upstream destination extracted from the proxy path.
type PathTarget struct {
	// Scheme is the upstream scheme, "http" or "https".
	Scheme string
	// Host is the upstream host: a DNS name, an IPv4 literal, or an IPv6
	// literal without brackets (brackets are stripped during parsing).
	Host string
	// Port is the explicit port or the scheme default (http: 80, https: 443).
	Port int
	// RawPath is the upstream path passed through as raw bytes (leading "/"
	// included). It is never decoded or re-encoded. Empty targets normalize to "/".
	RawPath string
}

// HostPort returns the "host:port" authority form, bracketing IPv6 literals.
func (t PathTarget) HostPort() string {
	if strings.Contains(t.Host, ":") {
		return "[" + t.Host + "]:" + strconv.Itoa(t.Port)
	}
	return t.Host + ":" + strconv.Itoa(t.Port)
}

// URL renders the upstream URL from raw parts without re-encoding RawPath.
// RawPath always carries a leading "/", so the root path renders as "/".
func (t PathTarget) URL() string {
	return t.Scheme + "://" + t.HostPort() + t.RawPath
}

// usageHint is attached to errors caused by malformed or missing targets so
// clients get the expected path shape in the error body. It names both
// accepted shapes (plain, leading +POLICY).
const usageHint = `expected path "[/+POLICY]/SCHEME/AUTHORITY[/PATH]" with SCHEME http or https and POLICY a retry policy like "status=5xx;*.attempts=3", e.g. "/https/api.example.com/v1/chat" or "/+status=5xx;*.attempts=3/https/api.example.com/v1/chat"; the policy may also be sent in the X-Reproxy-Retry-Policy header`

// ParsePath parses a proxy target and its optional leading-segment retry
// policy from the escaped request path (use r.URL.EscapedPath() so RAW-PATH
// bytes are preserved verbatim).
//
// Grammar:
//
//	PROXY-TARGET := [ "/" "+" POLICY ] "/" SCHEME "/" AUTHORITY [ "/" RAW-PATH ]
//
// The leading "/+"POLICY segment, when present, is a control token: reproxy's
// own surface. It is parsed first, with the shared pair grammar
// (parsePolicyPairList, segmentCarrier) on ORIGINAL bytes. SCHEME is then
// "http" or "https" (case-insensitive, lowercased) — a pure target that
// reports only the destination protocol. A nil params return means "no
// policy" — the only such spelling.
//
// The leading-segment dispatch is a single byte check at position 1, matched
// on the ORIGINAL escaped bytes: "%2B" is not "+" (no normalization
// re-interprets encoded characters as syntax), so "/%2Bstatus=5xx/https/host"
// fails scheme validation as the literal segment "%2Bstatus=5xx". The scheme
// part is case-insensitive; the policy body after the "/+" is matched on
// original bytes and never lowercased. A "+" anywhere else — including the
// scheme segment ("/https+status=5xx/host", the v0.3 spelling) or the target
// path ("/https/host/+x") — is ordinary path data: the former dies as an
// unsupported scheme, the latter is forwarded verbatim.
//
// SECOND-SYSTEM GUARDRAIL: the leading "+" segment speaks ONLY retry policy;
// the scheme segment is pure target. v0.3 welded policy onto the scheme
// token; that grammar died in v0.4 as an unsupported scheme. Nothing else may
// ever live in the "+" slot — no transport selectors, no feature flags, no
// additional namespaces. A future extension belongs in a new header or a new
// surface, not in this slot.
//
// Parsing is strict by design (SSRF layer L1): unknown schemes, malformed
// policies, userinfo, bracket-less IPv6, out-of-range or leading-zero
// ports, and malformed hosts are all rejected with a 400 RequestError naming
// the cause (and the raw segment, so typos are diagnosable).
func ParsePath(escapedPath string) (PathTarget, url.Values, *RequestError) {
	if escapedPath == "" || escapedPath == "/" {
		return PathTarget{}, nil, &RequestError{
			Code:   400,
			Reason: "missing upstream target in path",
			Hint:   usageHint,
		}
	}
	rest := strings.TrimPrefix(escapedPath, "/")

	// Leading control segment: "/+POLICY". One byte check at position 1 —
	// the same one-mechanism class as the rest of the grammar, one cut, no
	// lookahead. Absent means no policy from the path (nil params).
	var policyParams url.Values
	var pendingPolicy string
	if strings.HasPrefix(rest, "+") {
		var policySeg string
		if i := strings.Index(rest, "/"); i >= 0 {
			policySeg, rest = rest[1:i], rest[i+1:]
		} else {
			policySeg, rest = rest[1:], ""
		}
		if strings.TrimSpace(policySeg) == "" {
			// "/+" and "/+/https/h": the control segment is present but
			// empty (or whitespace-only) — degenerate, and absent is the only
			// no-policy spelling. The shared ladder's empty-body 400 fires
			// here (it always errors for an empty body, so the call is an
			// error-or-impossible form).
			if _, rerr := parsePolicyPairList(policySeg, segmentCarrier); rerr != nil {
				return PathTarget{}, nil, rerr
			}
		}
		if rest == "" {
			// "/+status=5xx" and nothing after it: the control segment
			// consumed the path, so the scheme never follows.
			return PathTarget{}, nil, &RequestError{
				Code:   400,
				Reason: "missing upstream scheme in path",
				Hint:   usageHint,
			}
		}
		pendingPolicy = policySeg
	}

	// Split off the scheme segment (first path segment before the next "/").
	var schemeSeg, remainder string
	if i := strings.Index(rest, "/"); i >= 0 {
		schemeSeg, remainder = rest[:i], rest[i+1:]
	} else {
		schemeSeg, remainder = rest, ""
	}
	scheme, rerr := parseSchemeSegment(schemeSeg)
	if rerr != nil {
		return PathTarget{}, nil, rerr
	}

	// The scheme survived its strict table: now the control segment's body
	// gets the shared pair grammar. Every degenerate form (empty pair,
	// whitespace-only, missing "=", empty key) is a 400 from the shared
	// fail-closed ladder; any bare word is the generic unknown-field 400.
	// This ordering keeps the inherited ladder precedence — scheme errors
	// name the scheme before policy errors name the field — so a second
	// "+"-shaped segment dies as an unsupported scheme, not a policy 400.
	if pendingPolicy != "" {
		params, rerr := parsePolicyPairList(pendingPolicy, segmentCarrier)
		if rerr != nil {
			return PathTarget{}, nil, rerr
		}
		policyParams = params
	}

	// Split off the authority (segment between scheme and raw path).
	var authority, rawPath string
	if i := strings.Index(remainder, "/"); i >= 0 {
		authority, rawPath = remainder[:i], remainder[i:]
	} else {
		authority = remainder
	}
	if authority == "" {
		return PathTarget{}, nil, &RequestError{
			Code:   400,
			Reason: "missing upstream authority in path",
			Hint:   usageHint,
		}
	}

	host, port, rerr := parseAuthority(authority)
	if rerr != nil {
		return PathTarget{}, nil, rerr
	}

	if rawPath == "" {
		rawPath = "/"
	}
	return PathTarget{Scheme: scheme, Host: host, Port: port, RawPath: rawPath}, policyParams, nil
}

// parseSchemeSegment validates the scheme segment against the strict table
// {http, https} — a PURE target: the segment reports only the destination
// protocol, nothing else. The scheme is case-insensitive (lowercased). There
// is no "+" logic here: a "+" in the segment (the v0.3 weld spelling
// "https+status=5xx", the bare control word "+", anything else) simply fails
// the strict table and dies as an unsupported scheme quoting the raw segment
// (no legacy detection — the v0.3 grammar is dead, not special-cased).
//
// Anything else — an unknown scheme — is a 400 whose reason names the actual
// malformed segment (so a typo reads as an unsupported scheme naming the
// input) and whose hint carries the usage hint.
func parseSchemeSegment(segment string) (scheme string, rerr *RequestError) {
	lower := strings.ToLower(segment)
	if lower != "http" && lower != "https" {
		if segment == "" {
			// "//host/..." (or "/+P/" with nothing after it) — the segment
			// is empty, which the caller reports as a missing target.
			return "", &RequestError{
				Code:   400,
				Reason: "missing upstream target in path",
				Hint:   usageHint,
			}
		}
		return "", &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("unsupported scheme %q: only http and https are supported", segment),
			Hint:   usageHint,
		}
	}
	return lower, nil
}

// parseAuthority splits and validates the authority: host plus optional port.
func parseAuthority(authority string) (string, int, *RequestError) {
	if strings.Contains(authority, "@") {
		return "", 0, &RequestError{
			Code:   400,
			Reason: "userinfo is not allowed in the upstream authority",
			Hint:   "remove \"user:pass@\" from the target; pass credentials in an Authorization header instead",
		}
	}

	host, portStr, err := splitHostPort(authority)
	if err != nil {
		return "", 0, err
	}
	if host == "" {
		return "", 0, &RequestError{
			Code:   400,
			Reason: "empty upstream host",
			Hint:   usageHint,
		}
	}

	port := 0 // 0 = scheme default
	if strings.HasSuffix(authority, ":") && !strings.HasSuffix(host, ":") {
		// "host:" with an empty port (e.g. "/https/h:") is malformed.
		return "", 0, &RequestError{
			Code:   400,
			Reason: "empty port after \":\"",
			Hint:   "write the port as decimal digits (1-65535) or omit it for the scheme default",
		}
	}
	if portStr != "" {
		p, rerr := parsePort(portStr)
		if rerr != nil {
			return "", 0, rerr
		}
		port = p
	}

	if rerr := validateHost(host); rerr != nil {
		return "", 0, rerr
	}
	return host, port, nil
}

// splitHostPort splits "host[:port]" / "[v6]:port" without net.SplitHostPort's
// leniency toward bare bracketed forms with junk in them.
func splitHostPort(authority string) (host, port string, err *RequestError) {
	if i := strings.LastIndex(authority, "]"); i >= 0 {
		// IPv6 form: must open with "[" immediately.
		if !strings.HasPrefix(authority, "[") {
			return "", "", &RequestError{
				Code:   400,
				Reason: "malformed IPv6 authority: missing opening \"[\"",
				Hint:   "IPv6 literals must be written in brackets, e.g. /https/[::1]:8080/x",
			}
		}
		host = authority[1:i]
		rest := authority[i+1:]
		if rest == "" {
			return host, "", nil
		}
		if !strings.HasPrefix(rest, ":") {
			return "", "", &RequestError{
				Code:   400,
				Reason: fmt.Sprintf("malformed authority %q: unexpected %q after the IPv6 literal", authority, rest),
				Hint:   "IPv6 literals must be written as [addr] or [addr]:port",
			}
		}
		return host, rest[1:], nil
	}
	if i := strings.LastIndex(authority, ":"); i >= 0 {
		if strings.HasPrefix(authority, "[") {
			return "", "", &RequestError{
				Code:   400,
				Reason: "malformed IPv6 authority: missing closing \"]\"",
				Hint:   "IPv6 literals must be written as [addr] or [addr]:port",
			}
		}
		return authority[:i], authority[i+1:], nil
	}
	return authority, "", nil
}

// parsePort validates an explicit port: 1-65535, decimal only, no leading
// zeros (they are a classic octal-confusion and canonicalization hazard).
func parsePort(s string) (int, *RequestError) {
	if s == "" {
		return 0, &RequestError{
			Code:   400,
			Reason: "empty port after \":\"",
			Hint:   "write the port as decimal digits (1-65535) or omit it for the scheme default",
		}
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("invalid port %q: leading zeros are not allowed", s),
			Hint:   "write the port in plain decimal without leading zeros",
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, &RequestError{
				Code:   400,
				Reason: fmt.Sprintf("invalid port %q: digits only", s),
				Hint:   "write the port in plain decimal (1-65535)",
			}
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, &RequestError{
			Code:   400,
			Reason: fmt.Sprintf("port %q out of range: must be 1-65535", s),
			Hint:   "choose a port between 1 and 65535, or omit it for the scheme default",
		}
	}
	return n, nil
}

// validateHost accepts only DNS names, IPv4 literals, and IPv6 literals;
// all other shapes (empty labels, IP-ish garbage, etc.) are rejected.
func validateHost(host string) *RequestError {
	if host == "" {
		return &RequestError{
			Code:   400,
			Reason: "empty upstream host",
			Hint:   usageHint,
		}
	}
	if strings.Contains(host, ":") {
		// Bare IPv6 literal: brackets were required (and stripped for
		// well-formed input), so anything left with a colon must still
		// parse as IPv6.
		ip := net.ParseIP(host)
		if ip == nil || ip.To16() == nil {
			return &RequestError{
				Code:   400,
				Reason: fmt.Sprintf("invalid IPv6 literal %q", host),
				Hint:   "IPv6 literals must be written in brackets, e.g. /https/[2001:db8::1]/x",
			}
		}
		return nil
	}

	// IPv4: any all-digits-and-dots string must parse cleanly (reject
	// "1.2.3", "1.2.3.4.5", "01.02.03.04" style ambiguities).
	if strings.Contains(host, ".") && !strings.Contains(host, "-") {
		if isIPv4ish(host) {
			ip := net.ParseIP(host)
			if ip == nil || ip.To4() == nil {
				return &RequestError{
					Code:   400,
					Reason: fmt.Sprintf("invalid IPv4 address %q", host),
					Hint:   "IPv4 must be four decimal octets, e.g. 203.0.113.7",
				}
			}
			return nil
		}
	}

	// DNS name: labels of letters, digits, hyphen (hyphen not at the edges),
	// dot-separated. Underscores are rejected (not valid hostnames).
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return &RequestError{
				Code:   400,
				Reason: fmt.Sprintf("invalid host %q: empty label", host),
				Hint:   "hostnames are dot-separated non-empty labels",
			}
		}
		if !validDNSLabel(label) {
			return &RequestError{
				Code:   400,
				Reason: fmt.Sprintf("invalid host %q", host),
				Hint:   "hostnames may only contain letters, digits, and hyphens (hyphens not at label edges)",
			}
		}
	}
	return nil
}

// isIPv4ish reports whether s looks like an attempt at a dotted-quad.
func isIPv4ish(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	digitDots := true
	for _, p := range parts {
		if p == "" {
			return false
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				digitDots = false
			}
		}
	}
	return digitDots
}

// validDNSLabel checks one hostname label: [A-Za-z0-9] with internal hyphens,
// not starting or ending with a hyphen, and at most 63 bytes.
func validDNSLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// schemeDefaultPort returns the default port for a scheme ParsePath accepts.
func schemeDefaultPort(scheme string) int {
	if scheme == "https" {
		return 443
	}
	return 80
}

// Normalize fills in the scheme-default port when none was given explicitly.
// ParsePath keeps Port=0 for "not specified"; consumers call this (or use
// HostPort) when they need the concrete port.
func (t PathTarget) Normalize() PathTarget {
	if t.Port == 0 {
		t.Port = schemeDefaultPort(t.Scheme)
	}
	return t
}
