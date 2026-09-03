package reproxy

import (
	"flag"
	"strings"
	"testing"
	"time"
)

func TestNewDefaultConfig(t *testing.T) {
	cfg := NewDefaultConfig()
	if cfg.Listen != ":8080" {
		t.Errorf("Listen default = %q, want %q", cfg.Listen, ":8080")
	}
	if cfg.MaxAttempts != 10 {
		t.Errorf("MaxAttempts default = %d, want 10", cfg.MaxAttempts)
	}
	if cfg.MaxBudget != 30*time.Second {
		t.Errorf("MaxBudget default = %s, want 30s", cfg.MaxBudget)
	}
	if cfg.MaxBody != 10<<20 {
		t.Errorf("MaxBody default = %d, want %d", cfg.MaxBody, 10<<20)
	}
	if cfg.StrictBodyLimit {
		t.Error("StrictBodyLimit default should be false")
	}
	if cfg.DangerousAllowAll {
		t.Error("DangerousAllowAll default should be false")
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	cfg, err := ParseFlags(flag.NewFlagSet("t", flag.ContinueOnError), []string{"--dangerous-allow-all"})
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Listen != DefaultListen || cfg.MaxAttempts != DefaultMaxAttempts ||
		cfg.MaxBudget != DefaultMaxBudget || cfg.MaxBody != DefaultMaxBody {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestParseFlagsOverrides(t *testing.T) {
	args := []string{
		"--listen", "127.0.0.1:9000",
		"--allowlist", "a.example.com, *.b.example.com",
		"--max-attempts", "5",
		"--max-budget", "2m",
		"--max-body", "1024",
		"--strict-body-limit",
	}
	cfg, err := ParseFlags(flag.NewFlagSet("t", flag.ContinueOnError), args)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9000" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if len(cfg.Allowlist) != 2 || cfg.Allowlist[0] != "a.example.com" || cfg.Allowlist[1] != "*.b.example.com" {
		t.Errorf("Allowlist = %v", cfg.Allowlist)
	}
	if cfg.MaxAttempts != 5 || cfg.MaxBudget != 2*time.Minute || cfg.MaxBody != 1024 || !cfg.StrictBodyLimit {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestParseFlagsRepeatableAllowlist(t *testing.T) {
	cfg, err := ParseFlags(flag.NewFlagSet("t", flag.ContinueOnError),
		[]string{"--allowlist", "a.com", "--allowlist", "b.com", "--dangerous-allow-all"})
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if len(cfg.Allowlist) != 2 {
		t.Errorf("Allowlist = %v, want 2 entries", cfg.Allowlist)
	}
}

func TestValidateRefusesEmptyAllowlistWithoutEscapeHatch(t *testing.T) {
	cfg := NewDefaultConfig()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected startup refusal with empty allowlist and no --dangerous-allow-all")
	}
	if !strings.Contains(err.Error(), "dangerous-allow-all") {
		t.Errorf("error should point at the escape hatch, got: %v", err)
	}

	cfg.DangerousAllowAll = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate with escape hatch: %v", err)
	}

	cfg.DangerousAllowAll = false
	cfg.Allowlist = []string{"example.com"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate with allowlist: %v", err)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*ServerConfig)
		want string
	}{
		{"attempts zero", func(c *ServerConfig) { c.MaxAttempts = 0 }, "max-attempts"},
		{"budget zero", func(c *ServerConfig) { c.MaxBudget = 0 }, "max-budget"},
		{"body negative", func(c *ServerConfig) { c.MaxBody = -1 }, "max-body"},
		{"empty entry", func(c *ServerConfig) { c.Allowlist = []string{""} }, "allowlist entry"},
		{"bare star", func(c *ServerConfig) { c.Allowlist = []string{"*"} }, "wildcard"},
		{"double star", func(c *ServerConfig) { c.Allowlist = []string{"a.*.b"} }, "wildcard"},
		{"mid star", func(c *ServerConfig) { c.Allowlist = []string{"*.a.*"} }, "wildcard"},
		{"star dot empty", func(c *ServerConfig) { c.Allowlist = []string{"*."} }, "wildcard"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewDefaultConfig()
			cfg.Allowlist = []string{"ok.example.com"} // baseline valid
			cfg.DangerousAllowAll = false
			tt.mut(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestAllows(t *testing.T) {
	newCfg := func(entries ...string) *ServerConfig {
		return &ServerConfig{Allowlist: entries}
	}
	tests := []struct {
		entries []string
		host    string
		want    bool
	}{
		// Exact matching.
		{[]string{"example.com"}, "example.com", true},
		{[]string{"example.com"}, "api.example.com", false},
		{[]string{"api.example.com"}, "example.com", false},
		{[]string{"example.com", "other.com"}, "other.com", true},
		// Case-insensitivity and trailing-dot normalization.
		{[]string{"EXAMPLE.com"}, "example.COM", true},
		{[]string{"example.com"}, "example.com.", true},
		// Single leading label wildcard, dot-anchored.
		{[]string{"*.example.com"}, "a.example.com", true},
		{[]string{"*.example.com"}, "example.com", false},
		{[]string{"*.example.com"}, "a.b.example.com", false},
		{[]string{"*.example.com"}, "aexample.com", false},
		{[]string{"*.example.com"}, "a.example.com.", true},
		{[]string{"*.example.com"}, "", false},
		// Exact entries never act as wildcards.
		{[]string{"example.com"}, "xexample.com", false},
		// Host port never reaches Allows, but empty host is denied.
		{[]string{"*.example.com"}, ".", false},
	}
	for _, tt := range tests {
		cfg := newCfg(tt.entries...)
		if got := cfg.Allows(tt.host); got != tt.want {
			t.Errorf("Allows(%q) with %v = %v, want %v", tt.host, tt.entries, got, tt.want)
		}
	}
}

func TestAllowsDangerousAllowAllBypassesList(t *testing.T) {
	cfg := &ServerConfig{Allowlist: []string{"example.com"}, DangerousAllowAll: true}
	if !cfg.Allows("anything.internal") {
		t.Error("Allows with --dangerous-allow-all should always be true")
	}
}
