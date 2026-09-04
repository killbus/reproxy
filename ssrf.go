package reproxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// forbiddenIPv4Prefixes lists every IPv4 range the proxy must never dial
// (SSRF layer L3/L4). Cloud metadata (169.254.0.0/16), loopback, RFC 1918
// private space, CGNAT, benchmark/test/documentation ranges, multicast, and
// reserved space are all rejected: a public-facing proxy has no business
// reaching any of them.
var forbiddenIPv4Prefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network" (unspecified-ish)
	netip.MustParsePrefix("10.0.0.0/8"),      // RFC 1918 private
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT shared address space
	netip.MustParsePrefix("127.0.0.0/8"),     // loopback
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local incl. cloud metadata
	netip.MustParsePrefix("172.16.0.0/12"),   // RFC 1918 private
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 relay anycast (deprecated)
	netip.MustParsePrefix("192.168.0.0/16"),  // RFC 1918 private
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved (incl. 255.255.255.255)
}

// forbiddenIPv6Prefixes lists IPv6 ranges rejected outright (no embedded-v4
// interpretation needed): the unspecified and loopback addresses, unique
// local addresses, link-local, multicast, and documentation space.
var forbiddenIPv6Prefixes = []netip.Prefix{
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),      // unique local addresses
	netip.MustParsePrefix("fe80::/10"),     // link-local
	netip.MustParsePrefix("ff00::/8"),      // multicast
	netip.MustParsePrefix("2001:db8::/32"), // documentation
}

// derivedIPv6Prefixes carry an embedded IPv4 address in bytes 4-7 of the
// 16-byte form. The embedded address is checked against the IPv4 list, so a
// mapped/translated presentation of a forbidden IPv4 address cannot sneak
// past the v4 checks (defense in depth against 4-in-6 bypass).
var derivedIPv6Prefixes = []netip.Prefix{
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 well-known prefix
	netip.MustParsePrefix("2002::/16"),    // 6to4
}

// isForbiddenIP reports whether addr must never be dialed. It consults the
// IPv4 list for 4-byte addresses and 4-in-6 mapped forms (after Unmap), the
// IPv6 list for native v6 addresses, and the derived-prefix list (NAT64/6to4)
// by checking the embedded IPv4 address. Zones are ignored so a scoped
// link-local address (fe80::1%eth0) is still caught.
func isForbiddenIP(addr netip.Addr) bool {
	addr = addr.WithZone("")

	// 4-in-6 mapped forms collapse to their IPv4 address; the v4 list then
	// decides (so ::ffff:8.8.8.8 is allowed, ::ffff:10.0.0.1 is not).
	if addr.Is4In6() {
		addr = addr.Unmap()
	}

	if addr.Is4() {
		return isForbiddenIPv4(addr)
	}

	// The unspecified address is forbidden in both families (netip reports
	// :: as Is4In6 false, Is6 true, so it lands here).
	if !addr.IsValid() || addr.IsUnspecified() {
		return true
	}

	for _, p := range forbiddenIPv6Prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	for _, p := range derivedIPv6Prefixes {
		if p.Contains(addr) {
			v4, ok := embeddedIPv4(addr)
			if !ok {
				return true // cannot interpret: conservative rejection
			}
			if isForbiddenIPv4(v4) {
				return true
			}
		}
	}
	return false
}

// isForbiddenIPv4 checks a 4-byte address against the IPv4 table.
func isForbiddenIPv4(addr netip.Addr) bool {
	for _, p := range forbiddenIPv4Prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// embeddedIPv4 extracts the IPv4 address embedded in a NAT64
// (64:ff9b::/96, low 32 bits) or 6to4 (2002::/16, bytes 2-5) address.
func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	b := addr.As16()
	var v4Bytes []byte
	if b[0] == 0x20 && b[1] == 0x02 {
		v4Bytes = b[2:6] // 6to4: 2002:<v4 hex>::
	} else {
		v4Bytes = b[12:16] // NAT64: 64:ff9b::<v4>
	}
	v4, ok := netip.AddrFromSlice(v4Bytes)
	if !ok || !v4.Is4() {
		return netip.Addr{}, false
	}
	return v4, true
}

// lookupFunc resolves a hostname to candidate IP addresses. It is a type
// field so tests can inject a mock resolver without touching the network.
type lookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// defaultLookup resolves via the stdlib resolver. LookupIPAddr honors the
// context, so DNS work is bounded by the request's lifecycle.
func defaultLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, ip := range addrs {
		if a, ok := netip.AddrFromSlice(ip.IP); ok {
			out = append(out, a)
		}
	}
	return out, nil
}

// dialFunc dials a validated address. Injectable for tests so the dial layer
// can be observed without real connections.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// defaultDial is the production dialer.
func defaultDial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// pinnedIPsCtxKey carries the validated IP set for one outbound request.
type pinnedIPsCtxKey struct{}

// WithPinnedIPs returns a context that carries the resolve-then-pin set for
// this request. The proxy layer resolves and validates once, then hands the
// pinned IPs to the transport; the DialContext never re-resolves, which
// closes the DNS-rebinding TOCTOU window.
func WithPinnedIPs(ctx context.Context, ips []netip.Addr) context.Context {
	return context.WithValue(ctx, pinnedIPsCtxKey{}, ips)
}

// pinnedIPsFromContext extracts the pinned IP set, reporting whether one was
// present.
func pinnedIPsFromContext(ctx context.Context) ([]netip.Addr, bool) {
	ips, ok := ctx.Value(pinnedIPsCtxKey{}).([]netip.Addr)
	return ips, ok
}

// PinnedResolver implements SSRF layers L3 (resolve-then-pin) and L4 (dial
// re-assert) on top of a shared *http.Transport:
//
//   - ResolveAndValidate resolves a host and validates EVERY returned
//     address against the forbidden list; any hit rejects the whole request
//     (fail closed).
//   - DialContext dials one of the pinned IPs directly (never re-resolving),
//     re-asserts isForbiddenIP on the address it is about to dial, and
//     leaves TLS ServerName to the transport (which uses the outbound URL's
//     hostname, preserving SNI and certificate validation).
type PinnedResolver struct {
	// Lookup resolves hostnames. Defaults to the stdlib resolver.
	Lookup lookupFunc
	// Dial connects to a validated address. Defaults to net.Dialer.
	Dial dialFunc
	// Transport is the shared transport wired to this resolver's DialContext.
	// Connection pooling is the transport's own; no per-host tuning beyond
	// the SSRF pin.
	Transport *http.Transport
}

// NewPinnedResolver builds a PinnedResolver with production defaults and its
// own shared transport. The transport never consults environment proxies
// (Proxy is nil): all outbound traffic goes through the pinned dialer only.
func NewPinnedResolver() *PinnedResolver {
	pr := &PinnedResolver{
		Lookup: defaultLookup,
		Dial:   defaultDial,
	}
	pr.Transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           pr.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return pr
}

// ResolveAndValidate resolves host and validates every returned address.
// IP-literal hosts skip DNS entirely and are validated directly. ANY
// forbidden address in the result rejects the whole request (fail closed):
// a hostname that resolves to both a public and a private address is treated
// as hostile, since the dialer could pick either.
func (pr *PinnedResolver) ResolveAndValidate(ctx context.Context, host, port string) ([]netip.Addr, *RequestError) {
	// IP literal: no DNS, direct validation.
	if ip, err := netip.ParseAddr(host); err == nil {
		if isForbiddenIP(ip) {
			return nil, forbiddenIPError(ip.String(), host, port)
		}
		return []netip.Addr{ip}, nil
	}

	addrs, err := pr.Lookup(ctx, host)
	if err != nil {
		return nil, &RequestError{
			Code:   502,
			Reason: fmt.Sprintf("failed resolving upstream host %q: %v", host, err),
			Hint:   "check the hostname; if it is correct, the resolver may be unavailable",
		}
	}
	if len(addrs) == 0 {
		return nil, &RequestError{
			Code:   502,
			Reason: fmt.Sprintf("upstream host %q resolved to no addresses", host),
			Hint:   "check the hostname; if it is correct, the resolver may be misconfigured",
		}
	}

	pinned := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if isForbiddenIP(a) {
			return nil, forbiddenIPError(a.String(), host, port)
		}
		pinned = append(pinned, a.WithZone(""))
	}
	return pinned, nil
}

// forbiddenIPError renders the L3 rejection. The resolved address is named
// so operators can diagnose allowlist/DNS issues from the response alone.
func forbiddenIPError(resolved, host, port string) *RequestError {
	return &RequestError{
		Code:   403,
		Reason: fmt.Sprintf("upstream %q resolves to a forbidden address %s (private, loopback, link-local, or otherwise reserved)", joinHostPort(host, port), resolved),
		Hint:   "this proxy only forwards to public upstream addresses; add the host to --allowlist only if reaching it is intended and safe",
	}
}

// joinHostPort brackets IPv6 literals for display.
func joinHostPort(host, port string) string {
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

// DialContext is the transport's dialer: it dials one of the pinned IPs
// carried in the request context (never re-resolving), re-asserts the
// forbidden check on the exact address about to be dialed (L4, defense in
// depth against an L3 bug), and dials it directly with the port from addr.
func (pr *PinnedResolver) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("reproxy dial: malformed address %q: %w", addr, err)
	}

	pinned, ok := pinnedIPsFromContext(ctx)
	if !ok {
		// No pinned set on the context (a caller bypassed the proxy layer's
		// resolve-then-pin). Resolve inline rather than dial unvalidated.
		resolved, rerr := pr.ResolveAndValidate(ctx, host, port)
		if rerr != nil {
			return nil, fmt.Errorf("reproxy dial: %s", rerr.Error())
		}
		pinned = resolved
		ctx = WithPinnedIPs(ctx, pinned)
	}

	var lastErr error
	for _, ip := range pinned {
		// L4: re-assert the forbidden check on the address being dialed.
		if isForbiddenIP(ip) {
			return nil, fmt.Errorf("reproxy dial: refusing forbidden address %s (L4 assertion)", ip)
		}
		// Honor explicit family requests from the transport (tcp4/tcp6).
		dialNetwork := network
		if network == "tcp4" && ip.Is6() {
			continue
		}
		if network == "tcp6" && ip.Is4() {
			continue
		}
		conn, derr := pr.Dial(ctx, dialNetwork, net.JoinHostPort(ip.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("reproxy dial: no dialable pinned address for %q", host)
	}
	return nil, lastErr
}
