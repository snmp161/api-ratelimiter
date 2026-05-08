package config

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

type Config struct {
	Listen                 string
	SocketMode             string
	AdminListen            string
	MetricsListen          string
	RedisAddr              string
	RedisPassword          string
	LogLevel               string
	LogFormat              string
	GlobalLimit            int
	Burst                  int
	Window                 string
	CleanupInterval        int
	AbuseTTL               int
	AbuseMultiplier        int
	AbuseTransferThreshold int
}

func Default() *Config {
	return &Config{
		Listen:                 "unix:/run/ratelimit.sock",
		SocketMode:             "0666",
		AdminListen:            "127.0.0.1:8080",
		MetricsListen:          "127.0.0.1:9091",
		RedisAddr:              "127.0.0.1:6379",
		RedisPassword:          "",
		LogLevel:               "info",
		LogFormat:              "json",
		GlobalLimit:            100,
		Burst:                  0,
		Window:                 "second",
		CleanupInterval:        15,
		AbuseTTL:               15,
		AbuseMultiplier:        10,
		AbuseTransferThreshold: 3,
	}
}

func (c *Config) BindFlags(fs *pflag.FlagSet) {
	fs.StringVar(&c.Listen, "listen", c.Listen, "Address for auth_request: unix:/path/sock or host:port")
	fs.StringVar(&c.SocketMode, "socket-mode", c.SocketMode, "File mode for unix socket from --listen, octal (e.g. 0666). Ignored for TCP.")
	fs.StringVar(&c.AdminListen, "admin-listen", c.AdminListen, "Address for web admin")
	fs.StringVar(&c.MetricsListen, "metrics-listen", c.MetricsListen, "Address for Prometheus metrics (/metrics)")
	fs.StringVar(&c.RedisAddr, "redis-addr", c.RedisAddr, "Redis address")
	fs.StringVar(&c.RedisPassword, "redis-password", c.RedisPassword, "Redis password")
	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "Log level: debug, info, warn, error")
	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat, "Log format: json or text")
	fs.IntVar(&c.GlobalLimit, "global-limit", c.GlobalLimit, "Global request limit per window")
	fs.IntVar(&c.Burst, "burst", c.Burst, "Extra requests over the limit (burst)")
	fs.StringVar(&c.Window, "window", c.Window, "Window unit: second or minute")
	fs.IntVar(&c.CleanupInterval, "cleanup-interval", c.CleanupInterval, "Cleanup interval in minutes")
	fs.IntVar(&c.AbuseTTL, "abuse-ttl", c.AbuseTTL, "TTL for redisDB2/redisDB3 entries, in minutes")
	fs.IntVar(&c.AbuseMultiplier, "abuse-multiplier", c.AbuseMultiplier, "Multiplier of global-limit for AbuseHits counting")
	fs.IntVar(&c.AbuseTransferThreshold, "abuse-transfer-threshold", c.AbuseTransferThreshold, "Min AbuseHits to transfer counter to redisDB2/redisDB3")
}

func (c *Config) Validate() error {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log-level %q (expected debug|info|warn|error)", c.LogLevel)
	}

	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("invalid log-format %q (expected json|text)", c.LogFormat)
	}

	switch c.Window {
	case "second", "minute":
	default:
		return fmt.Errorf("invalid window %q (expected second|minute)", c.Window)
	}

	if c.GlobalLimit <= 0 {
		return fmt.Errorf("global-limit must be > 0, got %d", c.GlobalLimit)
	}
	if c.Burst < 0 {
		return fmt.Errorf("burst must be >= 0, got %d", c.Burst)
	}
	if c.CleanupInterval <= 0 {
		return fmt.Errorf("cleanup-interval must be > 0, got %d", c.CleanupInterval)
	}
	if c.AbuseTTL <= 0 {
		return fmt.Errorf("abuse-ttl must be > 0, got %d", c.AbuseTTL)
	}
	if c.AbuseMultiplier <= 0 {
		return fmt.Errorf("abuse-multiplier must be > 0, got %d", c.AbuseMultiplier)
	}
	if c.AbuseTransferThreshold <= 0 {
		return fmt.Errorf("abuse-transfer-threshold must be > 0, got %d", c.AbuseTransferThreshold)
	}

	if c.Burst >= c.GlobalLimit*c.AbuseMultiplier {
		return fmt.Errorf("--burst (%d) must be < --global-limit (%d) * --abuse-multiplier (%d) = %d",
			c.Burst, c.GlobalLimit, c.AbuseMultiplier, c.GlobalLimit*c.AbuseMultiplier)
	}

	// Socket mode is consumed only when --listen is a unix socket but
	// validate the value regardless so a typo isn't silently ignored
	// after switching --listen.
	if _, err := c.SocketModeParsed(); err != nil {
		return fmt.Errorf("invalid --socket-mode %q: %w", c.SocketMode, err)
	}

	return nil
}

// SocketModeParsed parses --socket-mode as octal. Both "0666" and "666"
// are accepted (base 8 ignores the leading zero).
func (c *Config) SocketModeParsed() (fs.FileMode, error) {
	s := strings.TrimSpace(c.SocketMode)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, err
	}
	if v > 0o777 {
		return 0, fmt.Errorf("must be ≤ 0777")
	}
	return fs.FileMode(v), nil
}

// IsUnixListen reports whether --listen is a unix socket spec.
func (c *Config) IsUnixListen() bool {
	return strings.HasPrefix(c.Listen, "unix:")
}

func (c *Config) WindowSeconds() int64 {
	if c.Window == "minute" {
		return 60
	}
	return 1
}

func (c *Config) WindowDuration() time.Duration {
	return time.Duration(c.WindowSeconds()) * time.Second
}

func (c *Config) CleanupDuration() time.Duration {
	return time.Duration(c.CleanupInterval) * time.Minute
}

func (c *Config) AbuseTTLDuration() time.Duration {
	return time.Duration(c.AbuseTTL) * time.Minute
}
