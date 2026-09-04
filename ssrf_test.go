package reproxy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"
)

// forbiddenIPv4Cases covers EVERY IPv4 prefix in forbiddenIPv4Prefixes with
// at least one address inside it (PRD acceptance item: full list coverage).
func forbiddenIPv4Cases() map[string][]string {
	return map[string][]string{
		"0.0.0.0/8":       {"0.0.0.0", "0.1.2.3", "0.255.255.255"},
		"10.0.0.0/8":      {"10.0.0.1", "10.1.2.3", "10.255.255.255"},
		"100.64.0.0/10":   {"100.64.0.1", "100.100.100.100", "100.127.255.255"},
		"127.0.0.0/8":     {"127.0.0.1", "127.8.8.8", "127.255.255.255"},
		"169.254.0.0/16":  {"169.254.169.254", "169.254.0.1", "169.254.255.255"},
		"172.16.0.0/12":   {"172.16.0.1", "172.31.255.254", "172.20.10.5"},
		"192.0.0.0/24":    {"192.0.0.1", "192.0.0.255"},
		"192.0.2.0/24":    {"192.0.2.1", "192.0.2.255"},
		"192.88.99.0/24":  {"192.88.99.1", "192.88.99.100"},
		"192.168.0.0/16":  {"192.168.0.1", "192.168.1.1", "192.168.255.255"},
		"198.18.0.0/15":   {"198.18.0.1", "198.19.255.254"},
		"198.51.100.0/24": {"198.51.100.1", "198.51.100.100"},
		"203.0.113.0/24":  {"203.0.113.1", "203.0.113.255"},
		"224.0.0.0/4":     {"224.0.0.1", "239.1.1.1"},
		"240.0.0.0/4":     {"240.0.0.1", "255.255.255.255", "250.1.2.3"},
	}
}

// TestIsForbiddenIPv4FullList walks the full v4 table: every range must be
// covered by at least one address, every listed address must be rejected,
// and the boundary neighbors must pass.
func TestIsForbiddenIPv4FullList(t *testing.T) {
	covered := map[string]bool{}
	for prefix, addrs := range forbiddenIPv4Cases() {
		covered[prefix] = true
		for _, s := range addrs {
			if !isForbiddenIP(netip.MustParseAddr(s)) {
				t.Errorf("isForbiddenIP(%s) = false, want true (in %s)", s, prefix)
			}
		}
	}
	for _, p := range forbiddenIPv4Prefixes {
		if !covered[p.String()] {
			t.Errorf("prefix %s has no test case coverage", p)
		}
	}
}

// TestIsForbiddenIPv4Boundaries checks addresses just outside the forbidden
// ranges: they must NOT be rejected (over-blocking is also a correctness bug).
func TestIsForbiddenIPv4Boundaries(t *testing.T) {
	allowed := []string{
		"1.0.0.1",         // after 0.0.0.0/8
		"9.255.255.255",   // before 10.0.0.0/8
		"11.0.0.1",        // after 10.0.0.0/8
		"100.63.255.255",  // before CGNAT
		"100.128.0.1",     // after CGNAT
		"126.255.255.255", // before loopback
		"128.0.0.1",       // after loopback
		"169.253.255.255", // before link-local
		"169.255.0.1",     // after link-local
		"172.15.255.255",  // before 172.16/12
		"172.32.0.1",      // after 172.16/12
		"191.255.255.255", // before 192.168/16
		"193.0.0.1",       // general public
		"198.17.255.255",  // before benchmark
		"198.20.0.1",      // after benchmark
		"223.255.255.255", // before multicast
	}
	for _, s := range allowed {
		if isForbiddenIP(netip.MustParseAddr(s)) {
			t.Errorf("isForbiddenIP(%s) = true, want false (public)", s)
		}
	}
}

// TestIsForbiddenIPv6FullList covers every IPv6 prefix in
// forbiddenIPv6Prefixes plus the unspecified address.
func TestIsForbiddenIPv6FullList(t *testing.T) {
	forbidden := map[string][]string{
		"::1/128":       {"::1"},
		"fc00::/7":      {"fc00::1", "fdff::1", "fd12:3456:789a::1"},
		"fe80::/10":     {"fe80::1", "febf::1", "fe80::1%eth0"},
		"ff00::/8":      {"ff02::1", "ffff::1"},
		"2001:db8::/32": {"2001:db8::1", "2001:db8:ffff::1"},
	}
	covered := map[string]bool{}
	for prefix, addrs := range forbidden {
		covered[prefix] = true
		for _, s := range addrs {
			a, err := netip.ParseAddr(s)
			if err != nil {
				t.Fatalf("ParseAddr(%q): %v", s, err)
			}
			if !isForbiddenIP(a) {
				t.Errorf("isForbiddenIP(%s) = false, want true (in %s)", s, prefix)
			}
		}
	}
	for _, p := range forbiddenIPv6Prefixes {
		if !covered[p.String()] {
			t.Errorf("prefix %s has no test case coverage", p)
		}
	}
	// The unspecified address is forbidden in both families.
	if !isForbiddenIP(netip.MustParseAddr("::")) {
		t.Errorf("isForbiddenIP(::) = false, want true")
	}
}

// TestIsForbiddenIPv6Boundaries checks public IPv6 passes.
func TestIsForbiddenIPv6Boundaries(t *testing.T) {
	allowed := []string{
		"2606:4700::1111",
		"2001:4860:4860::8888",
		"2620:fe::fe",
	}
	for _, s := range allowed {
		if isForbiddenIP(netip.MustParseAddr(s)) {
			t.Errorf("isForbiddenIP(%s) = true, want false (public)", s)
		}
	}
}

// TestIsForbiddenIPMappedAndDerived covers 4-in-6 mapped forms and
// embedded-v4 derived prefixes (NAT64, 6to4): a forbidden IPv4 in any of
// these presentations must not bypass the v4 checks; a public one passes.
func TestIsForbiddenIPMappedAndDerived(t *testing.T) {
	forbidden := []string{
		"::ffff:10.0.0.1",        // v4-mapped private
		"::ffff:127.0.0.1",       // v4-mapped loopback
		"::ffff:169.254.169.254", // v4-mapped metadata
		"::ffff:0.0.0.0",         // v4-mapped this-network
		"::ffff:192.168.1.1",     // v4-mapped RFC1918
		"64:ff9b::10.0.0.1",      // NAT64 wrapping private v4
		"64:ff9b::127.0.0.1",     // NAT64 wrapping loopback
		"2002:0a00:0001::1",      // 6to4 wrapping 10.0.0.1
		"2002:7f00:0001::1",      // 6to4 wrapping 127.0.0.1
	}
	for _, s := range forbidden {
		if !isForbiddenIP(netip.MustParseAddr(s)) {
			t.Errorf("isForbiddenIP(%s) = false, want true (embedded forbidden v4)", s)
		}
	}

	allowed := []string{
		"::ffff:8.8.8.8",    // v4-mapped public
		"::ffff:1.1.1.1",    // v4-mapped public
		"64:ff9b::8.8.8.8",  // NAT64 wrapping public v4
		"2002:0808:0808::1", // 6to4 wrapping 8.8.8.8
	}
	for _, s := range allowed {
		if isForbiddenIP(netip.MustParseAddr(s)) {
			t.Errorf("isForbiddenIP(%s) = true, want false (embedded public v4)", s)
		}
	}
}

// TestIsForbiddenIPPublicGlobal covers the PRD's named public addresses.
func TestIsForbiddenIPPublicGlobal(t *testing.T) {
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111"} {
		if isForbiddenIP(netip.MustParseAddr(s)) {
			t.Errorf("isForbiddenIP(%s) = true, want false", s)
		}
	}
}

// TestIsForbiddenIPInvalid rejects the zero-value address.
func TestIsForbiddenIPInvalid(t *testing.T) {
	if !isForbiddenIP(netip.Addr{}) {
		t.Errorf("isForbiddenIP(zero Addr) = false, want true")
	}
}

// mockLookupBuilder returns a lookupFunc yielding exactly the given IPs.
func mockLookupBuilder(ips ...string) lookupFunc {
	return func(ctx context.Context, host string) ([]netip.Addr, error) {
		out := make([]netip.Addr, 0, len(ips))
		for _, s := range ips {
			out = append(out, netip.MustParseAddr(s))
		}
		return out, nil
	}
}

// TestResolveAndValidateIPLiteral checks that IP-literal targets skip DNS and
// are validated directly: a private literal is 403 without any lookup, and a
// public literal pins without any lookup.
func TestResolveAndValidateIPLiteral(t *testing.T) {
	called := false
	pr := &PinnedResolver{
		Lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			called = true
			return nil, nil
		},
	}

	if _, err := pr.ResolveAndValidate(context.Background(), "10.1.2.3", "80"); err == nil {
		t.Fatal("ResolveAndValidate(10.1.2.3) = nil error, want 403")
	} else if err.Code != 403 {
		t.Errorf("code = %d, want 403", err.Code)
	}
	if called {
		t.Error("IP-literal target must not trigger DNS resolution")
	}

	ips, err := pr.ResolveAndValidate(context.Background(), "8.8.8.8", "80")
	if err != nil {
		t.Fatalf("ResolveAndValidate(8.8.8.8): %v", err)
	}
	if len(ips) != 1 || ips[0] != netip.MustParseAddr("8.8.8.8") {
		t.Errorf("pinned = %v, want [8.8.8.8]", ips)
	}
	if called {
		t.Error("IP-literal target must not trigger DNS resolution")
	}

	// IPv6 literal in brackets is stripped before reaching here.
	if _, err := pr.ResolveAndValidate(context.Background(), "::1", "80"); err == nil {
		t.Fatal("ResolveAndValidate(::1) = nil error, want 403")
	}
}

// TestResolveAndValidateMixedResults checks fail-closed semantics: a host
// resolving to [public, private] is rejected; [public, public] pins both.
func TestResolveAndValidateMixedResults(t *testing.T) {
	pr := &PinnedResolver{Lookup: mockLookupBuilder("8.8.8.8", "10.0.0.1")}
	if _, err := pr.ResolveAndValidate(context.Background(), "evil.example.com", "443"); err == nil {
		t.Fatal("mixed [public, private] resolution must be rejected")
	} else if err.Code != 403 {
		t.Errorf("code = %d, want 403", err.Code)
	}

	pr = &PinnedResolver{Lookup: mockLookupBuilder("1.1.1.1", "8.8.8.8")}
	ips, err := pr.ResolveAndValidate(context.Background(), "ok.example.com", "443")
	if err != nil {
		t.Fatalf("all-public resolution failed: %v", err)
	}
	if len(ips) != 2 {
		t.Errorf("pinned count = %d, want 2", len(ips))
	}
}

// TestResolveAndValidatePrivateOnly rejects a purely private resolution.
func TestResolveAndValidatePrivateOnly(t *testing.T) {
	pr := &PinnedResolver{Lookup: mockLookupBuilder("192.168.1.1")}
	_, err := pr.ResolveAndValidate(context.Background(), "intranet.local", "80")
	if err == nil {
		t.Fatal("private-only resolution must be rejected")
	}
	if err.Code != 403 {
		t.Errorf("code = %d, want 403", err.Code)
	}
}

// TestResolveAndValidateLookupError surfaces resolver failures as 502 with
// the host named in the reason.
func TestResolveAndValidateLookupError(t *testing.T) {
	pr := &PinnedResolver{
		Lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return nil, fmt.Errorf("NXDOMAIN")
		},
	}
	_, err := pr.ResolveAndValidate(context.Background(), "gone.example.com", "80")
	if err == nil {
		t.Fatal("lookup error must surface")
	}
	if err.Code != 502 {
		t.Errorf("code = %d, want 502", err.Code)
	}
}

// TestResolveAndValidateEmptyResult rejects a resolver that returns nothing.
func TestResolveAndValidateEmptyResult(t *testing.T) {
	pr := &PinnedResolver{Lookup: mockLookupBuilder()}
	_, err := pr.ResolveAndValidate(context.Background(), "void.example.com", "80")
	if err == nil {
		t.Fatal("empty resolution must be rejected")
	}
	if err.Code != 502 {
		t.Errorf("code = %d, want 502", err.Code)
	}
}

// TestResolveAndValidateZonesStripped ensures scoped addresses (with zones)
// are normalized when pinned.
func TestResolveAndValidateZonesStripped(t *testing.T) {
	pr := &PinnedResolver{
		Lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("2606:4700::1111%eth0")}, nil
		},
	}
	ips, err := pr.ResolveAndValidate(context.Background(), "z.example.com", "443")
	if err != nil {
		t.Fatalf("scoped public address must resolve: %v", err)
	}
	if ips[0].Zone() != "" {
		t.Errorf("pinned address retains zone %q, want stripped", ips[0].Zone())
	}
}

// recordingDial captures every dial attempt for assertion.
type recordingDial struct {
	calls []string
}

func (d *recordingDial) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.calls = append(d.calls, network+" "+addr)
	return nil, fmt.Errorf("recording dial always fails (test)")
}

// TestDialContextDialsPinnedIP asserts the dial layer dials the validated IP
// (not the hostname) and honors the port from the transport address.
func TestDialContextDialsPinnedIP(t *testing.T) {
	rec := &recordingDial{}
	pr := &PinnedResolver{Lookup: mockLookupBuilder("93.184.216.10"), Dial: rec.dial}
	ips, rerr := pr.ResolveAndValidate(context.Background(), "up.example.com", "8080")
	if rerr != nil {
		t.Fatalf("ResolveAndValidate: %v", rerr)
	}
	ctx := WithPinnedIPs(context.Background(), ips)
	_, err := pr.DialContext(ctx, "tcp", "up.example.com:8080")
	if err == nil {
		t.Fatal("DialContext with failing dialer must error")
	}
	if len(rec.calls) != 1 || rec.calls[0] != "tcp 93.184.216.10:8080" {
		t.Errorf("dials = %v, want [tcp 93.184.216.10:8080]", rec.calls)
	}
}

// TestDialContextReassertsForbidden ensures the L4 assertion fires even when
// the pinned set was tampered with (defense in depth against an L3 bug).
func TestDialContextReassertsForbidden(t *testing.T) {
	rec := &recordingDial{}
	pr := &PinnedResolver{Dial: rec.dial}
	// Simulate an L3 bug: a forbidden address ends up in the pinned set.
	ctx := WithPinnedIPs(context.Background(), []netip.Addr{netip.MustParseAddr("169.254.169.254")})
	_, err := pr.DialContext(ctx, "tcp", "meta.example.com:80")
	if err == nil {
		t.Fatal("DialContext must refuse a forbidden pinned address")
	}
	if len(rec.calls) != 0 {
		t.Errorf("forbidden address was dialed: %v", rec.calls)
	}
}

// TestDialContextNoPinResolvesInline checks that a context without a pinned
// set (a caller bypassing resolve-then-pin) still resolves and validates
// inline rather than dialing unvalidated.
func TestDialContextNoPinResolvesInline(t *testing.T) {
	rec := &recordingDial{}
	pr := &PinnedResolver{Lookup: mockLookupBuilder("10.0.0.5"), Dial: rec.dial}
	_, err := pr.DialContext(context.Background(), "tcp", "bypass.example.com:80")
	if err == nil {
		t.Fatal("DialContext without pinned set must not dial a private host")
	}
	if len(rec.calls) != 0 {
		t.Errorf("unvalidated dial happened: %v", rec.calls)
	}

	// Public host without a pinned set: inline resolution pins then dials.
	pr.Lookup = mockLookupBuilder("93.184.216.20")
	_, _ = pr.DialContext(context.Background(), "tcp", "ok.example.com:80")
	if len(rec.calls) != 1 || rec.calls[0] != "tcp 93.184.216.20:80" {
		t.Errorf("dials = %v, want [tcp 93.184.216.20:80]", rec.calls)
	}
}

// TestDialContextFallsThroughCandidates checks that a failed candidate IP is
// followed by the next pinned IP (connection failover within the pin set).
func TestDialContextFallsThroughCandidates(t *testing.T) {
	var calls []string
	pr := &PinnedResolver{
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			calls = append(calls, addr)
			if len(calls) == 1 {
				return nil, fmt.Errorf("connection refused (first)")
			}
			return nil, fmt.Errorf("connection refused (second)")
		},
	}
	ips := []netip.Addr{netip.MustParseAddr("93.184.216.1"), netip.MustParseAddr("93.184.216.2")}
	ctx := WithPinnedIPs(context.Background(), ips)
	_, err := pr.DialContext(ctx, "tcp", "up.example.com:80")
	if err == nil {
		t.Fatal("all candidates failing must surface an error")
	}
	if len(calls) != 2 {
		t.Errorf("dial attempts = %d, want 2", len(calls))
	}
}

// TestDialContextFamilyMismatch checks that family constraints from the
// transport (tcp4/tcp6) skip mismatched pinned candidates.
func TestDialContextFamilyMismatch(t *testing.T) {
	rec := &recordingDial{}
	pr := &PinnedResolver{Dial: rec.dial}
	ips := []netip.Addr{netip.MustParseAddr("2606:4700::1111")}
	ctx := WithPinnedIPs(context.Background(), ips)
	_, err := pr.DialContext(ctx, "tcp4", "up.example.com:80")
	if err == nil {
		t.Fatal("no matching-family candidate must error")
	}
	if len(rec.calls) != 0 {
		t.Errorf("mismatched family was dialed: %v", rec.calls)
	}
}
