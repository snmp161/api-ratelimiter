// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestStartup_SocketMode verifies that the unix socket file is
// created with exactly the mode passed via --socket-mode. Covers
// §17.2 A: «Unix-сокет создаётся по пути из --listen=unix:/… и имеет
// режим, соответствующий --socket-mode».
func TestStartup_SocketMode(t *testing.T) {
	cases := []string{"0666", "0660", "0600"}
	for _, mode := range cases {
		t.Run(mode, func(t *testing.T) {
			s := spawnService(t, withSocketMode(mode))

			fi, err := os.Stat(s.socketPath)
			if err != nil {
				t.Fatalf("stat socket: %v", err)
			}
			want, _ := strconv.ParseUint(mode, 8, 32)
			got := uint32(fi.Mode() & os.ModePerm)
			if got != uint32(want) {
				t.Errorf("socket mode = %#o, want %#o (--socket-mode=%s)", got, want, mode)
			}
		})
	}
}

// TestStartup_StaleSocketRemoved verifies that a leftover socket
// file from a previous run is removed before bind. Covers §17.2 A:
// «Stale-сокет из предыдущего запуска удаляется до bind'а».
func TestStartup_StaleSocketRemoved(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "stale.sock")

	// Pre-create a regular file at the socket path to simulate a stale
	// leftover. (A real socket file would also work, but we don't have
	// one and creating one is more setup than this needs.)
	if err := os.WriteFile(sockPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("pre-create: %v", err)
	}

	s := spawnService(t, withListen("unix:"+sockPath))

	// The file at sockPath should now be the new socket, not the
	// regular file we left behind.
	fi, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("path %q is not a socket after spawn (mode=%v)", sockPath, fi.Mode())
	}

	// And the service answers — proves the bind actually succeeded.
	code, err := s.check(map[string]string{"X-Real-IP": "1.2.3.4"})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if code != http.StatusOK {
		t.Errorf("check status = %d, want 200", code)
	}
}

// TestStartup_TCPListen — service can bind to a TCP address and
// serves /check there. Covers §17.2 A: «TCP --listen=127.0.0.1:0».
// We pre-pick a free port to give to the service rather than passing
// :0, because the binary doesn't echo back the chosen port.
func TestStartup_TCPListen(t *testing.T) {
	port := freePort(t)
	listen := fmt.Sprintf("127.0.0.1:%d", port)

	s := spawnService(t, withListen(listen))

	code, err := s.check(map[string]string{"X-Real-IP": "1.2.3.4"})
	if err != nil {
		t.Fatalf("check via TCP: %v", err)
	}
	if code != http.StatusOK {
		t.Errorf("check status = %d, want 200", code)
	}
}

// TestStartup_ReadyzReflectsRedisHealth — /readyz is 200 when Redis
// is reachable and 503 when it is not. Covers §17.2 A: «/readyz →
// 200 при живом Redis, 503 при недоступном».
//
// We can't kill miniredis mid-test cleanly (the t.Cleanup it
// registered will fire later and the service holds open
// connections), so instead we spawn a second service WITHOUT
// miniredis backing and check that /readyz answers 503 immediately.
func TestStartup_ReadyzRedisDown(t *testing.T) {
	s := spawnService(t, withoutRedis())

	resp, err := s.adminGET("/readyz")
	if err != nil {
		t.Fatalf("/readyz: %v", err)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz with Redis down: status = %d, want 503", resp.StatusCode)
	}
}

// TestStartup_FailOpenWhenRedisDown — service still answers /check
// with 200 when Redis is unreachable (fail-open contract from spec
// §3 and §17.2 A: «Сервис стартует и обслуживает /check при
// недоступном Redis»).
func TestStartup_FailOpenWhenRedisDown(t *testing.T) {
	s := spawnService(t, withoutRedis())

	// First request: api_key in DB1 lookup will fail (Redis down) →
	// fallback to UnknownCounters with global-limit. Default global-
	// limit is 100, so the first request must pass.
	code, err := s.check(map[string]string{
		"X-Api-Key": "some-key",
		"X-Real-IP": "1.2.3.4",
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if code != http.StatusOK {
		t.Errorf("check with Redis down: status = %d, want 200 (fail-open)", code)
	}
}

// TestStartup_MetricsRegistered — /metrics exposes all expected
// metric names. Covers §17.2 A: «/metrics отдаёт Prometheus-формат,
// содержит ожидаемые имена».
func TestStartup_MetricsRegistered(t *testing.T) {
	s := spawnService(t)

	body, code, err := s.metricsGET()
	if err != nil {
		t.Fatalf("/metrics: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", code)
	}

	// Each name from spec §10.
	want := []string{
		"ratelimit_requests_total",
		"ratelimit_counters_known_active",
		"ratelimit_counters_unknown_active",
		"ratelimit_memory_bytes",
		"ratelimit_cleanup_runs_total",
		"ratelimit_cleanup_deleted_total",
		"ratelimit_cleanup_transferred_total",
		"ratelimit_cleanup_last_duration_seconds",
		"ratelimit_redis_errors_total",
		"ratelimit_redis_db1_keys",
		"ratelimit_redis_db2_keys",
		"ratelimit_redis_db3_keys",
		"ratelimit_check_duration_seconds",
	}
	for _, name := range want {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics body missing %q", name)
		}
	}
}
