package reproxy

import (
	"flag"
	"fmt"
	"strings"
	"time"
)

// ServerConfig holds the server-level settings parsed from CLI flags.
//
// Server-level values act as clamps (they can narrow, never widen, what a
// request-level retry policy asks for) and as the SSRF deployment gate (L7).
type ServerConfig struct {
	// Listen is the address the HTTP server binds to (host:port).
	Listen string
	// Allowlist is the set of permitted upstream destinations: exact
	// hostnames or wildcard entries of the form "*.example.com".
	Allowlist []string
	// MaxAttempts caps the total attempt count (including the first)
	// any request may ask for via retry[*].attempts / retry[NNN].attempts.
	MaxAttempts int
	// MaxBudget caps the total retry lifecycle duration a request may ask
	// for via retry.budget.
	MaxBudget time.Duration
	// MaxBody is the request body capture cap in bytes. Bodies larger than
	// this are not retried (degraded pass-through, or 413 in strict mode).
	MaxBody int64
	// StrictBodyLimit makes oversized request bodies a 413 error instead of
	// a degraded pass-through.
	StrictBodyLimit bool
	// DangerousAllowAll disables the destination allowlist (L2). The server
	// refuses to start with an empty allowlist unless this is set.
	DangerousAllowAll bool
}

// Defaults for ServerConfig fields.
const (
	DefaultListen      = ":8080"
	DefaultMaxAttempts = 10
	DefaultMaxBudget   = 30 * time.Second
	DefaultMaxBody     = 10 << 20 // 10 MiB — audit-recommended general-purpose default
)

// NewDefaultConfig returns a ServerConfig with all defaults applied.
func NewDefaultConfig() *ServerConfig {
	return &ServerConfig{
		Listen:      DefaultListen,
		MaxAttempts: DefaultMaxAttempts,
		MaxBudget:   DefaultMaxBudget,
		MaxBody:     DefaultMaxBody,
	}
}

// ParseFlags builds a ServerConfig from CLI arguments. It is exposed with an
// injected FlagSet so tests can drive it without touching os.Args.
//
// Unknown flags are reported by the FlagSet itself; semantic validation
// (including the L7 startup gate) happens in Validate.
func ParseFlags(fs *flag.FlagSet, args []string) (*ServerConfig, error) {
	cfg := NewDefaultConfig()

	fs.StringVar(&cfg.Listen, "listen", DefaultListen, "listen address (host:port)")
	fs.Func("allowlist", "comma-separated destination allowlist: exact hostnames and '*.suffix' wildcards (repeatable)", func(v string) error {
		for _, h := range strings.Split(v, ",") {
			h = strings.TrimSpace(h)
			if h != "" {
				cfg.Allowlist = append(cfg.Allowlist, h)
			}
		}
		return nil
	})
	fs.IntVar(&cfg.MaxAttempts, "max-attempts", DefaultMaxAttempts, "server cap on total attempts per request (>=1)")
	fs.DurationVar(&cfg.MaxBudget, "max-budget", DefaultMaxBudget, "server cap on total retry lifecycle duration (e.g. 30s)")
	fs.Int64Var(&cfg.MaxBody, "max-body", DefaultMaxBody, "request body capture cap in bytes; bodies over this are not retried (default 10 MiB)")
	fs.BoolVar(&cfg.StrictBodyLimit, "strict-body-limit", false, "reject oversized request bodies with 413 instead of degrading to pass-through")
	fs.BoolVar(&cfg.DangerousAllowAll, "dangerous-allow-all", false, "disable the destination allowlist (EXPOSES the proxy as an open relay)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks semantic constraints on the configuration and enforces the
// deployment-level access control gate (SSRF layer L7): the server refuses to
// start when the allowlist is empty and --dangerous-allow-all is not set.
func (c *ServerConfig) Validate() error {
	if c.MaxAttempts < 1 {
		return fmt.Errorf("max-attempts must be >= 1, got %d", c.MaxAttempts)
	}
	if c.MaxBudget <= 0 {
		return fmt.Errorf("max-budget must be > 0, got %s", c.MaxBudget)
	}
	if c.MaxBody < 0 {
		return fmt.Errorf("max-body must be >= 0, got %d", c.MaxBody)
	}
	if !c.DangerousAllowAll && len(c.Allowlist) == 0 {
		return fmt.Errorf("empty allowlist: set --allowlist or pass --dangerous-allow-all to run without destination restrictions")
	}
	for _, e := range c.Allowlist {
		if err := validateAllowlistEntry(e); err != nil {
			return err
		}
	}
	return nil
}

// validateAllowlistEntry rejects malformed allowlist entries early so a
// misconfigured wildcard cannot silently never match.
func validateAllowlistEntry(entry string) error {
	if entry == "" {
		return fmt.Errorf("allowlist entry must not be empty")
	}
	rest := strings.TrimPrefix(entry, "*.")
	if rest == "" || strings.ContainsAny(rest, "*") {
		return fmt.Errorf("invalid allowlist entry %q: wildcards are only supported as a leading \"*.\" prefix", entry)
	}
	return nil
}

// Allows reports whether host is permitted by the allowlist. Matching is
// case-insensitive (DNS names are). An entry matches either exactly, or as a
// single-label wildcard prefix (dot-anchored): "*.example.com" matches
// "a.example.com" but neither "example.com" nor "a.b.example.com".
func (c *ServerConfig) Allows(host string) bool {
	if c.DangerousAllowAll {
		return true
	}
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if h == "" {
		return false
	}
	for _, entry := range c.Allowlist {
		e := strings.ToLower(entry)
		if e == h {
			return true
		}
		if suffix, ok := strings.CutPrefix(e, "*."); ok {
			label, rest, found := strings.Cut(h, ".")
			if !found || label == "" {
				continue
			}
			if rest == suffix {
				return true
			}
		}
	}
	return false
}
