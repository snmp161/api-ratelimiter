// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"
	"time"
)

func TestValidate_BurstExceedsAbuseThreshold(t *testing.T) {
	c := Default()
	c.GlobalLimit = 10
	c.AbuseMultiplier = 10
	c.Burst = 100 // 100 >= 10*10
	if err := c.Validate(); err == nil {
		t.Fatal("expected validation error when burst >= global-limit * abuse-multiplier")
	}
}

func TestValidate_BurstAtBoundary(t *testing.T) {
	c := Default()
	c.GlobalLimit = 10
	c.AbuseMultiplier = 10
	c.Burst = 99 // 99 < 100
	if err := c.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidate_OK(t *testing.T) {
	c := Default()
	c.GlobalLimit = 100
	c.Burst = 20
	c.AbuseMultiplier = 10
	if err := c.Validate(); err != nil {
		t.Fatalf("default-ish config should validate: %v", err)
	}
}

func TestValidate_InvalidWindow(t *testing.T) {
	c := Default()
	c.Window = "hour"
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for invalid window")
	}
}

func TestValidate_InvalidLogLevel(t *testing.T) {
	c := Default()
	c.LogLevel = "trace"
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for invalid log level")
	}
}

func TestValidate_NonPositiveLimit(t *testing.T) {
	c := Default()
	c.GlobalLimit = 0
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for non-positive global-limit")
	}
}

func TestValidate_NegativeBurst(t *testing.T) {
	c := Default()
	c.Burst = -1
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for negative burst")
	}
}

func TestValidate_NonPositiveCleanupInterval(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		c := Default()
		c.CleanupInterval = d
		if err := c.Validate(); err == nil {
			t.Errorf("cleanup-interval=%v: expected validation error", d)
		}
	}
}

func TestValidate_NonPositiveAbuseTTL(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		c := Default()
		c.AbuseTTL = d
		if err := c.Validate(); err == nil {
			t.Errorf("abuse-ttl=%v: expected validation error", d)
		}
	}
}

func TestValidate_NonPositiveAbuseMultiplier(t *testing.T) {
	c := Default()
	c.AbuseMultiplier = 0
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for abuse-multiplier=0")
	}
}

func TestValidate_NonPositiveAbuseTransferThreshold(t *testing.T) {
	c := Default()
	c.AbuseTransferThreshold = 0
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for abuse-transfer-threshold=0")
	}
}

func TestValidate_InvalidLogFormat(t *testing.T) {
	c := Default()
	c.LogFormat = "xml"
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for invalid log-format")
	}
}

func TestDefault_DurationFields(t *testing.T) {
	c := Default()
	if c.CleanupInterval != 15*time.Minute {
		t.Errorf("CleanupInterval default = %v, want 15m", c.CleanupInterval)
	}
	if c.AbuseTTL != 15*time.Minute {
		t.Errorf("AbuseTTL default = %v, want 15m", c.AbuseTTL)
	}
}

func TestSocketModeParsed(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
		err  bool
	}{
		{"0666", 0o666, false},
		{"666", 0o666, false},
		{"0700", 0o700, false},
		{"777", 0o777, false},
		{"1000", 0, true}, // > 0o777
		{"abc", 0, true},
		{"", 0, true},
	}
	for _, tc := range cases {
		c := Default()
		c.SocketMode = tc.in
		mode, err := c.SocketModeParsed()
		if tc.err {
			if err == nil {
				t.Errorf("%q: expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if uint32(mode) != tc.want {
			t.Errorf("%q: got %o want %o", tc.in, mode, tc.want)
		}
	}
}

func TestValidate_BadSocketMode(t *testing.T) {
	c := Default()
	c.SocketMode = "1000"
	if err := c.Validate(); err == nil {
		t.Fatal("expected validation error for socket-mode > 0777")
	}
}

func TestWindowSeconds(t *testing.T) {
	c := Default()
	c.Window = "second"
	if c.WindowSeconds() != 1 {
		t.Errorf("second: want 1, got %d", c.WindowSeconds())
	}
	c.Window = "minute"
	if c.WindowSeconds() != 60 {
		t.Errorf("minute: want 60, got %d", c.WindowSeconds())
	}
}
