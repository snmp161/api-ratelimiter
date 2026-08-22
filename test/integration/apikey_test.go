// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Covers §17.2 C — per-api-key limits managed in redisDB1.

// TestAPIKey_DB1LimitApplied — a key present in redisDB1 gets its own
// limit, and exceeding it is counted as blocked_individual.
func TestAPIKey_DB1LimitApplied(t *testing.T) {
	s := spawnService(t,
		withFlag("--global-limit", "100"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
	)
	s.seedLimit("abc", 3)

	start := time.Now()
	codes := s.checkN(4, map[string]string{"X-Api-Key": "abc"})
	if !sameWindow(start, time.Now(), 60) {
		t.Skip("requests straddled a minute boundary")
	}

	if got := countStatus(codes, http.StatusOK); got != 3 {
		t.Errorf("200s = %d, want 3 (redisDB1 limit) — statuses: %v", got, codes)
	}
	if codes[3] != http.StatusForbidden {
		t.Errorf("4th request: status = %d, want 403", codes[3])
	}
	if v := s.metric("ratelimit_requests_total", map[string]string{"result": "blocked_individual"}); v != 1 {
		t.Errorf("blocked_individual = %v, want 1", v)
	}
	if v := s.metric("ratelimit_requests_total", map[string]string{"result": "blocked_global"}); v != 0 {
		t.Errorf("blocked_global = %v, want 0 — the block came from the individual limit", v)
	}
}

// TestAPIKey_OutsideDB1UsesGlobalLimit — the same service, a key that
// is not in redisDB1: global limit, blocked_global.
func TestAPIKey_OutsideDB1UsesGlobalLimit(t *testing.T) {
	s := spawnService(t,
		withFlag("--global-limit", "2"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
	)
	s.seedLimit("privileged", 50)

	start := time.Now()
	unknown := s.checkN(3, map[string]string{"X-Api-Key": "plain"})
	privileged := s.checkN(3, map[string]string{"X-Api-Key": "privileged"})
	if !sameWindow(start, time.Now(), 60) {
		t.Skip("requests straddled a minute boundary")
	}

	if unknown[2] != http.StatusForbidden {
		t.Errorf("unknown key: statuses = %v, want the 3rd to be 403 (global limit 2)", unknown)
	}
	if got := countStatus(privileged, http.StatusOK); got != 3 {
		t.Errorf("privileged key: 200s = %d/3, want 3 (redisDB1 limit is 50)", got)
	}
	if v := s.metric("ratelimit_requests_total", map[string]string{"result": "blocked_global"}); v != 1 {
		t.Errorf("blocked_global = %v, want 1", v)
	}
	if v := s.metric("ratelimit_requests_total", map[string]string{"result": "blocked_individual"}); v != 0 {
		t.Errorf("blocked_individual = %v, want 0", v)
	}
}

// TestAPIKey_RemovedFromDB1AtRuntime — deleting the redisDB1 entry
// while the service runs: new requests immediately fall back to
// UnknownCounters with the global limit, and the stale KnownCounter is
// dropped by the next cleanup cycle (spec §5, cleanup.runKnown).
func TestAPIKey_RemovedFromDB1AtRuntime(t *testing.T) {
	const key = "runtime-removed-key"
	s := spawnService(t,
		withFlag("--global-limit", "100"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
		withFlag("--cleanup-interval", "500ms"),
	)
	s.seedLimit(key, 1)

	if code := s.mustCheck(map[string]string{"X-Api-Key": key}); code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", code)
	}
	if code := s.mustCheck(map[string]string{"X-Api-Key": key}); code != http.StatusForbidden {
		t.Fatalf("second request: status = %d, want 403 (individual limit 1)", code)
	}

	// The KnownCounter is live right now — it must be visible in the
	// in-memory top table on /limits.
	if _, body := s.adminBody("/limits"); !strings.Contains(body, key) {
		t.Fatalf("/limits does not show the KnownCounter before removal")
	}

	if err := s.db(1).Del(context.Background(), "rate:limit:"+key).Err(); err != nil {
		t.Fatalf("delete rate:limit:%s: %v", key, err)
	}

	// Fallback is immediate: the lookup misses, so the request is
	// counted in UnknownCounters against the global limit of 100.
	if code := s.mustCheck(map[string]string{"X-Api-Key": key}); code != http.StatusOK {
		t.Errorf("after redisDB1 removal: status = %d, want 200 (global limit applies)", code)
	}

	// …and the stale KnownCounter is dropped by cleanup, which logs the
	// reason before deleting.
	s.waitLog("api_key removed from redisDB1", 5*time.Second)
	waitFor(t, 5*time.Second, "stale KnownCounter to disappear from /limits", func() bool {
		_, body := s.adminBody("/limits")
		// The UnknownCounter created by the fallback request lives on
		// /abuse/keys, so /limits mentioning the key at all means the
		// KnownCounter is still there.
		return !strings.Contains(body, key)
	})
}

// TestAPIKey_RedisDownFallsBackToGlobal — losing Redis mid-flight must
// not turn an api_key request into a hard failure: the lookup error
// routes it to UnknownCounters with the global limit (invariant 3).
func TestAPIKey_RedisDownFallsBackToGlobal(t *testing.T) {
	const key = "fallback-key"
	rdb := newCtlRedis(t)
	s := spawnService(t,
		withRedisAddr(rdb.addr),
		withFlag("--global-limit", "100"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
	)
	s.seedLimit(key, 1)

	if code := s.mustCheck(map[string]string{"X-Api-Key": key}); code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", code)
	}
	if code := s.mustCheck(map[string]string{"X-Api-Key": key}); code != http.StatusForbidden {
		t.Fatalf("second request: status = %d, want 403 (individual limit 1)", code)
	}

	rdb.stop()

	// The individual limit is unreachable, so the key gets the global
	// limit of 100 — the previously exhausted counter no longer applies.
	codes := s.checkN(3, map[string]string{"X-Api-Key": key})
	if got := countStatus(codes, http.StatusOK); got != 3 {
		t.Errorf("with Redis down: statuses = %v, want three 200s (fail open to global limit)", codes)
	}
	if v := s.metric("ratelimit_redis_errors_total", nil); v < 1 {
		t.Errorf("ratelimit_redis_errors_total = %v, want ≥ 1 after the lookups failed", v)
	}
	if code, _ := s.adminBody("/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz with Redis down: status = %d, want 503", code)
	}
}
