package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const validTOML = `
[network]
ipv6_pool = "2001:db8:abcd::/64"
[auth]
enabled = true
user = "alice"
pass = "s3cret"
`

func TestLoadValid(t *testing.T) {
	cfg, err := Load(writeTemp(t, validTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got, want := cfg.Pool().String(), "2001:db8:abcd::/64"; got != want {
		t.Errorf("pool = %q, want %q", got, want)
	}
	if cfg.Server.Listen != ":28888" {
		t.Errorf("listen = %q, want :28888 default", cfg.Server.Listen)
	}
	if cfg.Network.DialTimeout.Std() != 10*time.Second {
		t.Errorf("dial_timeout = %v, want 10s default", cfg.Network.DialTimeout.Std())
	}
	if cfg.Dedup.TTL.Std() != 24*time.Hour {
		t.Errorf("dedup.ttl = %v, want 24h default", cfg.Dedup.TTL.Std())
	}
	if !cfg.Auth.Enabled {
		t.Error("auth.enabled default should be true when omitted from a file that sets other auth keys")
	}
}

func TestValidateRejectsBadPools(t *testing.T) {
	for _, tc := range []struct{ name, pool, wantSub string }{
		{"ipv4", "10.0.0.0/8", "is not an IPv6 prefix"},
		{"not a cidr", "2001:db8::", "not a valid CIDR prefix"},
		{"wrong bits", "2001:db8:abcd::/48", "must be a /64 prefix, got /48"},
		{"not masked", "2001:db8:abcd::1/64", "is not masked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Network.IPv6Pool = tc.pool
			cfg.Auth = AuthConfig{Enabled: true, User: "u", Pass: "p"}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate(%q) = nil, want error", tc.pool)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err, tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.pool) {
				t.Errorf("error %q does not name the offending value %q", err, tc.pool)
			}
		})
	}
}

func TestValidateAuthRequiresCredentials(t *testing.T) {
	cfg := Default()
	cfg.Network.IPv6Pool = "2001:db8::/64"
	cfg.Auth = AuthConfig{Enabled: true, User: "alice"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate = nil, want error for empty auth.pass")
	}
	if !strings.Contains(err.Error(), "auth.pass") {
		t.Errorf("error %q should name auth.pass", err)
	}

	cfg.Auth = AuthConfig{Enabled: false}
	if err := cfg.Validate(); err != nil {
		t.Errorf("disabled auth with empty credentials: %v", err)
	}
}

func TestEnvOverridesSecrets(t *testing.T) {
	t.Setenv("HE_IPV6_PROXY_AUTH_USER", "bob")
	t.Setenv("HE_IPV6_PROXY_AUTH_PASS", "envpass")
	cfg, err := Load(writeTemp(t, validTOML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.User != "bob" || cfg.Auth.Pass != "envpass" {
		t.Fatalf("auth = %q/%q, want bob/envpass", cfg.Auth.User, cfg.Auth.Pass)
	}

	// Empty env vars must leave the file values intact.
	t.Setenv("HE_IPV6_PROXY_AUTH_USER", "")
	t.Setenv("HE_IPV6_PROXY_AUTH_PASS", "")
	cfg, err = Load(writeTemp(t, validTOML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.User != "alice" || cfg.Auth.Pass != "s3cret" {
		t.Fatalf("auth = %q/%q, want alice/s3cret", cfg.Auth.User, cfg.Auth.Pass)
	}
}

func TestLogFileDefaultsAndValidation(t *testing.T) {
	cfg := Default()
	cfg.Network.IPv6Pool = "2001:db8::/64"
	cfg.Auth = AuthConfig{Enabled: true, User: "u", Pass: "p"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got, want := cfg.Log.Path(), filepath.Join("log", "he-ipv6-proxy.log"); got != want {
		t.Errorf("default log path = %q, want %q", got, want)
	}

	for _, bad := range []string{"sub/x.log", "/var/log/x.log", "..", "."} {
		cfg := Default()
		cfg.Network.IPv6Pool = "2001:db8::/64"
		cfg.Auth = AuthConfig{Enabled: true, User: "u", Pass: "p"}
		cfg.Log.File = bad
		err := cfg.Validate()
		if err == nil {
			t.Errorf("Validate accepted log.file %q", bad)
			continue
		}
		if !strings.Contains(err.Error(), "log.file") || !strings.Contains(err.Error(), bad) {
			t.Errorf("error %q should name log.file and %q", err, bad)
		}
	}
}

// A file that sets log.file = "" keeps file logging off: the empty value must
// survive validation instead of falling back to the default name.
func TestLogFileExplicitlyDisabled(t *testing.T) {
	cfg, err := Load(writeTemp(t, validTOML+"\n[log]\nfile = \"\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := cfg.Log.Path(); got != "" {
		t.Errorf("log path = %q, want file logging disabled", got)
	}
}

func TestValidateRejectsBadLogLevel(t *testing.T) {
	cfg := Default()
	cfg.Network.IPv6Pool = "2001:db8::/64"
	cfg.Auth = AuthConfig{Enabled: true, User: "u", Pass: "p"}
	cfg.Log.Level = "verbose"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted log.level = verbose")
	}
}

func TestValidateRejectsBadTestTarget(t *testing.T) {
	cfg := Default()
	cfg.Network.IPv6Pool = "2001:db8::/64"
	cfg.Auth = AuthConfig{Enabled: true, User: "u", Pass: "p"}
	cfg.Doctor.TestTarget = "not-a-hostport"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate = nil, want error for bad doctor.test_target")
	}
	cfg.Doctor.TestTarget = "[2001:db8::1]:80"
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid test target rejected: %v", err)
	}
}
