package reproxy

import (
	"strings"
	"testing"
)

func TestParsePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    PathTarget
		wantErr string // substring of Reason; empty means success expected
		wantHnt string // substring of Hint, checked when wantErr != ""
	}{
		// --- Missing / malformed target shape ---
		{"empty path", "", PathTarget{}, "missing upstream target", "SCHEME"},
		{"bare slash", "/", PathTarget{}, "missing upstream target", "SCHEME"},
		{"scheme only", "/https", PathTarget{}, "missing upstream authority", "SCHEME"},
		{"scheme and slash", "/https/", PathTarget{}, "missing upstream authority", "SCHEME"},
		{"double slash after host slash", "//example.com", PathTarget{}, "missing upstream target", "SCHEME"},
		{"unknown scheme ftp", "/ftp/example.com/x", PathTarget{}, "unsupported scheme", "SCHEME http or https"},
		{"scheme uppercase accepted", "/HTTPS/example.com", PathTarget{"https", "example.com", 0, "/"}, "", ""},

		// --- Valid simple targets ---
		{"https with path", "/https/api.example.com/v1/chat", PathTarget{"https", "api.example.com", 0, "/v1/chat"}, "", ""},
		{"http with path", "/http/h.dev:4000/v1/x", PathTarget{"http", "h.dev", 4000, "/v1/x"}, "", ""},
		{"empty path normalizes", "/https/example.com", PathTarget{"https", "example.com", 0, "/"}, "", ""},
		{"trailing slash equals root", "/https/example.com/", PathTarget{"https", "example.com", 0, "/"}, "", ""},
		{"scheme case insensitive", "/HTTP/example.com", PathTarget{"http", "example.com", 0, "/"}, "", ""},
		{"deep path preserved", "/https/h/a/b/c/d", PathTarget{"https", "h", 0, "/a/b/c/d"}, "", ""},
		{"empty segment in path preserved", "/https/h/a//b", PathTarget{"https", "h", 0, "/a//b"}, "", ""},
		{"path with colon", "/https/h/a:b", PathTarget{"https", "h", 0, "/a:b"}, "", ""},
		{"path with at-sign", "/https/h/x@y", PathTarget{"https", "h", 0, "/x@y"}, "", ""},
		{"encoded bytes preserved", "/https/h/a%2Fb%20c+d", PathTarget{"https", "h", 0, "/a%2Fb%20c+d"}, "", ""},
		{"percent in path preserved", "/https/h/%zz", PathTarget{"https", "h", 0, "/%zz"}, "", ""},

		// --- Ports ---
		{"explicit https port", "/https/h:8443/x", PathTarget{"https", "h", 8443, "/x"}, "", ""},
		{"port one", "/https/h:1", PathTarget{"https", "h", 1, "/"}, "", ""},
		{"port max", "/https/h:65535", PathTarget{"https", "h", 65535, "/"}, "", ""},
		{"port zero", "/https/h:0", PathTarget{}, "out of range", "1 and 65535"},
		{"port too big", "/https/h:65536", PathTarget{}, "out of range", "1 and 65535"},
		{"port huge", "/https/h:99999", PathTarget{}, "out of range", "1 and 65535"},
		{"port leading zero", "/https/h:080", PathTarget{}, "leading zeros", "leading zeros"},
		{"port leading zero four digits", "/https/h:0080", PathTarget{}, "leading zeros", "leading zeros"},
		{"port non-numeric", "/https/h:8o0", PathTarget{}, "digits only", "plain decimal"},
		{"port negative", "/https/h:-80", PathTarget{}, "digits only", "plain decimal"},
		{"trailing colon empty port", "/https/h:", PathTarget{}, "empty port", "decimal"},
		{"port with letters after digits", "/https/h:80a", PathTarget{}, "digits only", "1-65535"},

		// --- Userinfo forbidden ---
		{"userinfo basic", "/https/user:pass@example.com/", PathTarget{}, "userinfo", "Authorization"},
		{"userinfo bare user", "/https/user@example.com/", PathTarget{}, "userinfo", "Authorization"},
		{"userinfo in path not authority", "/https/h/u@v", PathTarget{"https", "h", 0, "/u@v"}, "", ""},

		// --- IPv4 ---
		{"ipv4 literal", "/http/127.0.0.1:8080/x", PathTarget{"http", "127.0.0.1", 8080, "/x"}, "", ""},
		{"ipv4 default port", "/http/203.0.113.7", PathTarget{"http", "203.0.113.7", 0, "/"}, "", ""},
		{"ipv4 bad octet", "/http/999.0.113.7", PathTarget{}, "invalid IPv4", "octets"},
		{"ipv4 three octets", "/http/1.2.3", PathTarget{}, "invalid IPv4", "octets"},
		{"ipv4 five octets", "/http/1.2.3.4.5", PathTarget{}, "invalid IPv4", "octets"},

		// --- IPv6 (brackets required) ---
		{"ipv6 loopback", "/https/[::1]:8443/x", PathTarget{"https", "::1", 8443, "/x"}, "", ""},
		{"ipv6 no port", "/https/[2001:db8::1]/x", PathTarget{"https", "2001:db8::1", 0, "/x"}, "", ""},
		{"ipv6 full", "/http/[::ffff:192.0.2.1]:80/x", PathTarget{"http", "::ffff:192.0.2.1", 80, "/x"}, "", ""},
		{"ipv6 bare rejected", "/https/::1/x", PathTarget{}, "invalid IPv6", "brackets"},
		{"ipv6 no brackets no port", "/https/2001:db8::1", PathTarget{}, "invalid IPv6", "brackets"},
		{"ipv6 junk after brackets", "/https/[::1]junk", PathTarget{}, "malformed authority", "[addr]:port"},
		{"ipv6 missing close bracket", "/https/[::1/x", PathTarget{}, "missing closing", "[addr]:port"},

		// --- Host shapes ---
		{"dns single label", "/https/localhost", PathTarget{"https", "localhost", 0, "/"}, "", ""},
		{"dns multi label", "/https/a.b.example.com", PathTarget{"https", "a.b.example.com", 0, "/"}, "", ""},
		{"dns with digits", "/https/h1.example.com", PathTarget{"https", "h1.example.com", 0, "/"}, "", ""},
		{"dns internal hyphen", "/https/my-host.example.com", PathTarget{"https", "my-host.example.com", 0, "/"}, "", ""},
		{"dns leading hyphen label", "/https/-host.example.com", PathTarget{}, "invalid host", "letters, digits"},
		{"dns trailing hyphen label", "/https/host-.example.com", PathTarget{}, "invalid host", "letters, digits"},
		{"dns underscore rejected", "/https/my_host.example.com", PathTarget{}, "invalid host", "letters, digits"},
		{"dns empty label double dot", "/https/a..b", PathTarget{}, "empty label", "non-empty"},
		{"dns empty label trailing dot", "/https/a.", PathTarget{}, "empty label", "non-empty"},
		{"dns empty label leading dot", "/https/.a", PathTarget{}, "empty label", "non-empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePath(tt.path)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParsePath(%q) = %+v, want error %q", tt.path, got, tt.wantErr)
				}
				if err.Code != 400 {
					t.Errorf("error code = %d, want 400", err.Code)
				}
				if !strings.Contains(err.Reason, tt.wantErr) {
					t.Errorf("error reason = %q, want substring %q", err.Reason, tt.wantErr)
				}
				if tt.wantHnt != "" && !strings.Contains(err.Hint, tt.wantHnt) {
					t.Errorf("error hint = %q, want substring %q", err.Hint, tt.wantHnt)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePath(%q) unexpected error: %v (hint: %s)", tt.path, err, err.Hint)
			}
			if got != tt.want {
				t.Errorf("ParsePath(%q) = %+v, want %+v", tt.path, got, tt.want)
			}
		})
	}
}

func TestPathTargetNormalizeAndRendering(t *testing.T) {
	tests := []struct {
		name         string
		in           PathTarget
		wantPort     int
		wantHostPort string
		wantURL      string
	}{
		{"http default", PathTarget{"http", "h", 0, "/"}, 80, "h:80", "http://h:80/"},
		{"https default", PathTarget{"https", "h", 0, "/"}, 443, "h:443", "https://h:443/"},
		{"explicit kept", PathTarget{"https", "h", 8443, "/x"}, 8443, "h:8443", "https://h:8443/x"},
		{"ipv6 bracketed in hostport", PathTarget{"https", "::1", 0, "/"}, 443, "[::1]:443", "https://[::1]:443/"},
		{"root path elided in url", PathTarget{"https", "h", 0, "/"}, 443, "h:443", "https://h:443/"},
		{"raw path preserved in url", PathTarget{"https", "h", 0, "/a%2Fb"}, 443, "h:443", "https://h:443/a%2Fb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := tt.in.Normalize()
			if n.Port != tt.wantPort {
				t.Errorf("Normalize() port = %d, want %d", n.Port, tt.wantPort)
			}
			if hp := n.HostPort(); hp != tt.wantHostPort {
				t.Errorf("HostPort() = %q, want %q", hp, tt.wantHostPort)
			}
			if u := n.URL(); u != tt.wantURL {
				t.Errorf("URL() = %q, want %q", u, tt.wantURL)
			}
		})
	}
}
