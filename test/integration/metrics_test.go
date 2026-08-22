// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"testing"
	"time"
)

// Covers §17.2 E — metric values after a known request sequence.

// TestMetrics_RequestCountsAreExact — 12 requests with a known split
// must produce exactly the expected counter values. --window=minute
// keeps the whole sequence inside one slot.
func TestMetrics_RequestCountsAreExact(t *testing.T) {
	s := spawnService(t,
		withFlag("--global-limit", "5"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
		withFlag("--cleanup-interval", "500ms"),
	)
	s.seedLimit("metered", 2)

	start := time.Now()
	s.checkN(8, map[string]string{"X-Real-IP": "192.0.2.10"}) // 5 allowed + 3 blocked_global
	s.checkN(4, map[string]string{"X-Api-Key": "metered"})    // 2 allowed + 2 blocked_individual
	if !sameWindow(start, time.Now(), 60) {
		t.Skip("requests straddled a minute boundary — counters reset mid-test")
	}

	checks := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"ratelimit_requests_total", map[string]string{"result": "allowed"}, 7},
		{"ratelimit_requests_total", map[string]string{"result": "blocked_global"}, 3},
		{"ratelimit_requests_total", map[string]string{"result": "blocked_individual"}, 2},
		// Every /check observation lands in the histogram.
		{"ratelimit_check_duration_seconds_count", nil, 12},
	}
	for _, c := range checks {
		if got := s.metric(c.name, c.labels); got != c.want {
			t.Errorf("%s = %v, want %v", seriesName(c.name, c.labels), got, c.want)
		}
	}

	// Spec §10: there is no aggregate result="blocked" label — the two
	// subtotals are summed in PromQL instead.
	if s.metricExists("ratelimit_requests_total", map[string]string{"result": "blocked"}) {
		t.Error(`ratelimit_requests_total{result="blocked"} exists, spec says it must not`)
	}
}

// TestMetrics_GaugesReflectState — the counter-map gauges, the memory
// estimate and the Redis key gauges are refreshed by the cleanup cycle.
func TestMetrics_GaugesReflectState(t *testing.T) {
	s := spawnService(t,
		withFlag("--global-limit", "100"),
		withFlag("--window", "minute"), // no counter goes inactive during the test
		withFlag("--cleanup-interval", "500ms"),
	)
	s.seedLimit("gauged", 10)
	s.seedAbuseKey("db2-entry")
	s.seedAbuseIP("198.51.100.1")

	s.mustCheck(map[string]string{"X-Api-Key": "gauged"})    // 1 KnownCounter
	s.mustCheck(map[string]string{"X-Real-IP": "192.0.2.1"}) // 1 UnknownCounter

	// Gauges are written at the end of a cleanup cycle.
	s.waitCleanupRuns(2, 5*time.Second)

	if got := s.metric("ratelimit_counters_known_active", nil); got != 1 {
		t.Errorf("ratelimit_counters_known_active = %v, want 1", got)
	}
	if got := s.metric("ratelimit_counters_unknown_active", nil); got != 1 {
		t.Errorf("ratelimit_counters_unknown_active = %v, want 1", got)
	}
	if got := s.metric("ratelimit_memory_bytes", nil); got <= 0 {
		t.Errorf("ratelimit_memory_bytes = %v, want > 0 with two live counters", got)
	}
	if got := s.metric("ratelimit_cleanup_runs_total", nil); got < 2 {
		t.Errorf("ratelimit_cleanup_runs_total = %v, want ≥ 2", got)
	}
	if got := s.metric("ratelimit_cleanup_last_duration_seconds", nil); got < 0 {
		t.Errorf("ratelimit_cleanup_last_duration_seconds = %v, want ≥ 0", got)
	}

	// The db*_keys gauges are refreshed from DBSize at the end of each
	// cleanup cycle, so poll rather than assume the cycle that ran after
	// seeding has already landed.
	dbGauges := []string{
		"ratelimit_redis_db1_keys",
		"ratelimit_redis_db2_keys",
		"ratelimit_redis_db3_keys",
	}
	waitFor(t, 5*time.Second, "redis db key gauges to catch up", func() bool {
		for _, name := range dbGauges {
			if s.metric(name, nil) != 1 {
				return false
			}
		}
		return true
	})
}

// TestMetrics_RedisErrorsCounted — a failed lookup on the hot path
// bumps ratelimit_redis_errors_total (and nothing else breaks).
func TestMetrics_RedisErrorsCounted(t *testing.T) {
	s := spawnService(t, withoutRedis(), withFlag("--window", "minute"))

	// The background DBSize gauge updater also counts Redis errors and
	// runs at startup, so compare against a baseline instead of zero.
	before := s.metric("ratelimit_redis_errors_total", nil)

	const n = 5
	s.checkN(n, map[string]string{"X-Api-Key": "some-key"})

	after := s.metric("ratelimit_redis_errors_total", nil)
	if after-before < n {
		t.Errorf("redis_errors_total delta = %v, want ≥ %d (one per failed lookup)", after-before, n)
	}
	// Fail-open: the requests themselves were allowed.
	if got := s.metric("ratelimit_requests_total", map[string]string{"result": "allowed"}); got != n {
		t.Errorf("allowed = %v, want %d — Redis errors must not block requests", got, n)
	}
}
