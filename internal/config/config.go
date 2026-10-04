// Package config loads and validates the he-ipv6-proxy TOML configuration.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration is a time.Duration that decodes from a TOML string such as "10s".
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler for TOML string values.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(text), err)
	}
	*d = Duration(v)
	return nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// ServerConfig holds the listener settings.
type ServerConfig struct {
	Listen            string   `toml:"listen"`
	ReadHeaderTimeout Duration `toml:"read_header_timeout"`
}

// NetworkConfig holds the IPv6 egress pool and upstream timeouts.
type NetworkConfig struct {
	IPv6Pool              string   `toml:"ipv6_pool"`
	DialTimeout           Duration `toml:"dial_timeout"`
	IdleTimeout           Duration `toml:"idle_timeout"`
	ResponseHeaderTimeout Duration `toml:"response_header_timeout"`
}

// AuthConfig holds the single shared proxy credential.
type AuthConfig struct {
	Enabled bool   `toml:"enabled"`
	User    string `toml:"user"`
	Pass    string `toml:"pass"`
}

// DedupConfig holds the source-address reuse window.
type DedupConfig struct {
	TTL           Duration `toml:"ttl"`
	SweepInterval Duration `toml:"sweep_interval"`
}

// LogConfig holds logging settings. Every line goes to stdout; when Dir and
// File are both non-empty the same lines are appended to <Dir>/<File>.
// Dir is resolved against the process working directory. An empty Dir or File
// turns file logging off.
type LogConfig struct {
	Level string `toml:"level"`
	Dir   string `toml:"dir"`
	File  string `toml:"file"`
}

// Path returns the log file path, or "" when file logging is disabled.
func (l LogConfig) Path() string {
	if l.Dir == "" || l.File == "" {
		return ""
	}
	return filepath.Join(l.Dir, l.File)
}

// RuntimeConfig holds process behaviour toggles.
type RuntimeConfig struct {
	SelfCheck bool `toml:"selfcheck"`
}

// DoctorConfig holds the optional live-dial probe target.
type DoctorConfig struct {
	TestTarget string `toml:"test_target"`
}

// Config is the whole configuration tree.
type Config struct {
	Server  ServerConfig  `toml:"server"`
	Network NetworkConfig `toml:"network"`
	Auth    AuthConfig    `toml:"auth"`
	Dedup   DedupConfig   `toml:"dedup"`
	Log     LogConfig     `toml:"log"`
	Runtime RuntimeConfig `toml:"runtime"`
	Doctor  DoctorConfig  `toml:"doctor"`
}

// Default returns a Config populated with the documented defaults. Decoding a
// TOML file over it leaves unspecified keys at these values.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Listen:            ":28888",
			ReadHeaderTimeout: Duration(30 * time.Second),
		},
		Network: NetworkConfig{
			DialTimeout:           Duration(10 * time.Second),
			IdleTimeout:           Duration(60 * time.Second),
			ResponseHeaderTimeout: Duration(30 * time.Second),
		},
		Auth: AuthConfig{Enabled: true},
		Dedup: DedupConfig{
			TTL:           Duration(24 * time.Hour),
			SweepInterval: Duration(5 * time.Minute),
		},
		Log:     LogConfig{Level: "info", Dir: "log", File: "he-ipv6-proxy.log"},
		Runtime: RuntimeConfig{SelfCheck: true},
	}
}

// Load reads the TOML file at path over the documented defaults and applies the
// HE_IPV6_PROXY_AUTH_USER / HE_IPV6_PROXY_AUTH_PASS environment overrides (only
// when the variable is non-empty). It does not validate; call Validate for that.
func Load(path string) (*Config, error) {
	cfg := Default()
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, err
	}
	if v := os.Getenv("HE_IPV6_PROXY_AUTH_USER"); v != "" {
		cfg.Auth.User = v
	}
	if v := os.Getenv("HE_IPV6_PROXY_AUTH_PASS"); v != "" {
		cfg.Auth.Pass = v
	}
	return cfg, nil
}

// Pool parses the configured IPv6 pool. Validate must have succeeded first.
func (c *Config) Pool() netip.Prefix {
	p, _ := netip.ParsePrefix(c.Network.IPv6Pool)
	return p
}

// Validate checks the configuration invariants and fills in zero-valued
// durations from the defaults. Errors name the offending value.
func (c *Config) Validate() error {
	if c.Server.Listen == "" {
		c.Server.Listen = Default().Server.Listen
	}
	if c.Server.ReadHeaderTimeout == 0 {
		c.Server.ReadHeaderTimeout = Default().Server.ReadHeaderTimeout
	}
	if c.Network.DialTimeout == 0 {
		c.Network.DialTimeout = Default().Network.DialTimeout
	}
	if c.Network.IdleTimeout == 0 {
		c.Network.IdleTimeout = Default().Network.IdleTimeout
	}
	if c.Network.ResponseHeaderTimeout == 0 {
		c.Network.ResponseHeaderTimeout = Default().Network.ResponseHeaderTimeout
	}
	if c.Dedup.TTL == 0 {
		c.Dedup.TTL = Default().Dedup.TTL
	}
	if c.Dedup.SweepInterval == 0 {
		c.Dedup.SweepInterval = Default().Dedup.SweepInterval
	}

	p, err := netip.ParsePrefix(c.Network.IPv6Pool)
	if err != nil {
		return fmt.Errorf("network.ipv6_pool %q is not a valid CIDR prefix: %w", c.Network.IPv6Pool, err)
	}
	if !p.IsValid() {
		return fmt.Errorf("network.ipv6_pool %q is not a valid CIDR prefix", c.Network.IPv6Pool)
	}
	if p != p.Masked() {
		return fmt.Errorf("network.ipv6_pool %q is not masked; use %q", c.Network.IPv6Pool, p.Masked())
	}
	if !p.Addr().Is6() || p.Addr().Is4In6() {
		return fmt.Errorf("network.ipv6_pool %q is not an IPv6 prefix", c.Network.IPv6Pool)
	}
	if p.Bits() != 64 {
		return fmt.Errorf("network.ipv6_pool %q must be a /64 prefix, got /%d", c.Network.IPv6Pool, p.Bits())
	}

	if c.Auth.Enabled {
		if c.Auth.User == "" {
			return fmt.Errorf("auth.enabled is true but auth.user is empty (set it in the config or HE_IPV6_PROXY_AUTH_USER)")
		}
		if c.Auth.Pass == "" {
			return fmt.Errorf("auth.enabled is true but auth.pass is empty (set it in the config or HE_IPV6_PROXY_AUTH_PASS)")
		}
	}

	switch c.Log.Level {
	case "", "info":
		c.Log.Level = "info"
	case "debug":
	default:
		return fmt.Errorf("log.level %q is not one of info|debug", c.Log.Level)
	}

	// The file name stays inside log.dir: a path separator would silently move
	// the log elsewhere (and out of the unit's writable path).
	if c.Log.File != "" && (strings.ContainsAny(c.Log.File, `/\`) || c.Log.File == "." || c.Log.File == "..") {
		return fmt.Errorf("log.file %q must be a bare file name; the directory goes in log.dir", c.Log.File)
	}

	if c.Doctor.TestTarget != "" {
		if _, err := netip.ParseAddrPort(c.Doctor.TestTarget); err != nil {
			return fmt.Errorf("doctor.test_target %q is not a valid IPv6 host:port: %w", c.Doctor.TestTarget, err)
		}
	}
	return nil
}
