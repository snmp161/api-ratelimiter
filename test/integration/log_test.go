// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestLog_InfoLevelHidesDebug — at --log-level=info the binary must
// emit INFO (the startup banner) but not DEBUG. Covers §17.2 A:
// «--log-level=info → INFO/WARN/ERROR в stdout, DEBUG нет».
func TestLog_InfoLevelHidesDebug(t *testing.T) {
	s := spawnService(t, withFlag("--log-level", "info"))
	// Send a /check — handler emits a DEBUG line per request. At
	// log-level=info this DEBUG must be suppressed.
	if _, err := s.check(map[string]string{"X-Real-IP": "1.2.3.4"}); err != nil {
		t.Fatalf("check: %v", err)
	}
	// Slog flushes synchronously, but the kernel pipe may still
	// buffer. A short sleep is the simplest deterministic-enough
	// signal here.
	time.Sleep(100 * time.Millisecond)

	out := s.stdoutSnapshot()
	if strings.Contains(out, "level=DEBUG") {
		t.Errorf("--log-level=info: stdout contains DEBUG lines\n%s", out)
	}
	if !strings.Contains(out, "level=INFO") {
		t.Errorf("--log-level=info: stdout missing INFO lines (no startup banner?)\n%s", out)
	}
}

// TestLog_DebugLevelEmitsDebug — at --log-level=debug the /check
// handler's per-request DEBUG line must appear.
func TestLog_DebugLevelEmitsDebug(t *testing.T) {
	// Default helper already passes --log-level=debug; we don't need
	// withFlag here, but be explicit so this test reads correctly.
	s := spawnService(t, withFlag("--log-level", "debug"))

	code, err := s.check(map[string]string{"X-Real-IP": "1.2.3.4"})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("check status = %d, want 200", code)
	}
	time.Sleep(100 * time.Millisecond)

	out := s.stdoutSnapshot()
	if !strings.Contains(out, "level=DEBUG") {
		t.Errorf("--log-level=debug: stdout missing DEBUG lines\n%s", out)
	}
}

// TestLog_JSONFormatValid — at --log-format=json every non-empty
// stdout line must be valid JSON.
func TestLog_JSONFormatValid(t *testing.T) {
	s := spawnService(t, withFlag("--log-format", "json"))

	// Generate at least one extra log line beyond the startup banner.
	if _, err := s.check(map[string]string{"X-Real-IP": "1.2.3.4"}); err != nil {
		t.Fatalf("check: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	out := s.stdoutSnapshot()
	if strings.TrimSpace(out) == "" {
		t.Fatal("stdout is empty — startup banner missing?")
	}

	lineNum := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lineNum++
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Errorf("non-JSON log line #%d: %q (err: %v)", lineNum, line, err)
		}
	}
	if lineNum == 0 {
		t.Error("no non-empty stdout lines to validate")
	}
}
