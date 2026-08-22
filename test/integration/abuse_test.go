// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"testing"
	"time"
)

// Covers §17.2 D — the cleanup cycle and the transfer of abusive
// UnknownCounters into redisDB2 / redisDB3.
//
// Shared configuration for these tests:
//
//	--window=second           slots are 1s wide, so abuse accrues in seconds
//	--global-limit=2 --burst=1  block above 3 requests per slot
//	--abuse-multiplier=3      AbuseHits++ when a slot saw > 6 requests
//	--abuse-transfer-threshold=2  transfer after two abusive slots
//	--cleanup-interval=500ms  cycles fast enough to observe
//
// requestsPerSlot is 8 — above the abuse threshold (6) so every slot
// counts as abusive, and above limit+burst (3) so burst_hits also gets
// exercised.
const requestsPerSlot = 8

func abuseFlags() []SpawnOpt {
	return []SpawnOpt{
		withFlag("--window", "second"),
		withFlag("--global-limit", "2"),
		withFlag("--burst", "1"),
		withFlag("--abuse-multiplier", "3"),
		withFlag("--abuse-transfer-threshold", "2"),
		withFlag("--cleanup-interval", "500ms"),
		withFlag("--abuse-ttl", "10s"),
		withFlag("--log-level", "info"),
	}
}

// hammer sends requestsPerSlot requests at the start of each of the
// next `slots` seconds. AbuseHits is incremented on slot change, so
// `slots` bursts produce slots-1 abuse hits. Returns the number of
// requests sent.
func hammer(s *service, headers map[string]string, slots int) int {
	s.t.Helper()
	sent := 0
	for i := 0; i < slots; i++ {
		sleepToNextSecond(20 * time.Millisecond)
		for j := 0; j < requestsPerSlot; j++ {
			s.mustCheck(headers)
			sent++
		}
	}
	return sent
}

// TestAbuse_IPTransferredAndGCd — the full lifecycle of an abusive IP:
// transferred to redisDB3 while still active (counter stays in memory),
// then GC'd from memory once it goes quiet, with the Redis record
// reflecting the final live state.
func TestAbuse_IPTransferredAndGCd(t *testing.T) {
	const ip = "203.0.113.5"
	s := spawnService(t, abuseFlags()...)

	before := time.Now().Unix()
	sent := hammer(s, map[string]string{"X-Real-IP": ip}, 3)

	key := "rate:abuse:ip:" + ip
	waitFor(t, 6*time.Second, "abusive IP to reach redisDB3", func() bool {
		_, ok := s.hgetAll(3, key)
		return ok
	})

	// Still active → still in memory (spec §5: active counters are
	// upserted but not deleted).
	if keys := s.topKeys("/abuse/ips"); !containsStr(keys, ip) {
		t.Errorf("in-memory top table = %v, want it to still contain %s (counter is active)", keys, ip)
	}

	if ttl := s.ttl(3, key); ttl <= 8*time.Second || ttl > 10*time.Second {
		t.Errorf("TTL = %v, want ≈ --abuse-ttl (10s)", ttl)
	}

	// Nothing must have leaked into the api-key database.
	if keys := s.keys(2, "rate:abuse:key:*"); len(keys) != 0 {
		t.Errorf("redisDB2 = %v, want empty (this counter is an IP)", keys)
	}

	// Stop the traffic: after 2×window of silence the counter is
	// inactive and the next cleanup drops it from memory. The Redis
	// record survives.
	waitFor(t, 10*time.Second, "inactive counter to be GC'd from memory", func() bool {
		return !containsStr(s.topKeys("/abuse/ips"), ip)
	})

	vals, ok := s.hgetAll(3, key)
	if !ok {
		t.Fatalf("%s disappeared from redisDB3 after GC", key)
	}
	after := time.Now().Unix()

	if got := hfieldInt(t, vals, "total_requests"); got != int64(sent) {
		t.Errorf("total_requests = %d, want %d (every request that hit the counter)", got, sent)
	}
	if got := hfieldInt(t, vals, "abuse_hits"); got < 2 {
		t.Errorf("abuse_hits = %d, want ≥ 2 (the transfer threshold)", got)
	}
	if got := hfieldInt(t, vals, "burst_hits"); got < 1 {
		t.Errorf("burst_hits = %d, want ≥ 1 (limit=2 burst=1, 8 requests per slot)", got)
	}
	first, last := hfieldInt(t, vals, "first_seen"), hfieldInt(t, vals, "last_seen")
	if first < before || first > after || last < first || last > after {
		t.Errorf("first_seen=%d last_seen=%d outside the test window [%d, %d]", first, last, before, after)
	}

	if v := s.metric("ratelimit_cleanup_transferred_total", nil); v < 1 {
		t.Errorf("ratelimit_cleanup_transferred_total = %v, want ≥ 1", v)
	}
	if v := s.metric("ratelimit_cleanup_deleted_total", nil); v < 1 {
		t.Errorf("ratelimit_cleanup_deleted_total = %v, want ≥ 1", v)
	}
	if v := s.metric("ratelimit_redis_db3_keys", nil); v < 1 {
		t.Errorf("ratelimit_redis_db3_keys = %v, want ≥ 1", v)
	}
}

// TestAbuse_KeyTransferredToDB2 — the same path for an api_key that is
// not in redisDB1: the record lands in redisDB2, keyed without the
// "key:" counter prefix.
func TestAbuse_KeyTransferredToDB2(t *testing.T) {
	const apiKey = "abusive-key"
	s := spawnService(t, abuseFlags()...)

	hammer(s, map[string]string{"X-Api-Key": apiKey}, 3)

	waitFor(t, 6*time.Second, "abusive api_key to reach redisDB2", func() bool {
		_, ok := s.hgetAll(2, "rate:abuse:key:"+apiKey)
		return ok
	})
	if keys := s.keys(3, "rate:abuse:ip:*"); len(keys) != 0 {
		t.Errorf("redisDB3 = %v, want empty (this counter is an api_key)", keys)
	}
	// The stored key must not carry the in-memory "key:" prefix.
	if keys := s.keys(2, "rate:abuse:key:*"); len(keys) != 1 || keys[0] != "rate:abuse:key:"+apiKey {
		t.Errorf("redisDB2 keys = %v, want [rate:abuse:key:%s]", keys, apiKey)
	}
}

// TestAbuse_InactiveNonAbusiveJustDeleted — a counter below the abuse
// threshold is GC'd from memory and never written to Redis.
func TestAbuse_InactiveNonAbusiveJustDeleted(t *testing.T) {
	const ip = "198.51.100.9"
	s := spawnService(t, abuseFlags()...)

	if code := s.mustCheck(map[string]string{"X-Real-IP": ip}); code != 200 {
		t.Fatalf("single request: status = %d, want 200", code)
	}
	if keys := s.topKeys("/abuse/ips"); !containsStr(keys, ip) {
		t.Fatalf("counter %s not visible in memory right after the request", ip)
	}

	waitFor(t, 10*time.Second, "inactive counter to be GC'd", func() bool {
		return !containsStr(s.topKeys("/abuse/ips"), ip)
	})
	if keys := s.keys(3, "rate:abuse:ip:*"); len(keys) != 0 {
		t.Errorf("redisDB3 = %v, want empty — the counter never crossed the abuse threshold", keys)
	}
}

// TestAbuse_KnownCounterNeverTransferred — a key with an individual
// limit in redisDB1 racks up violations but must never appear in
// redisDB2 (spec §5: KnownCounters are cleaned up without transfer).
func TestAbuse_KnownCounterNeverTransferred(t *testing.T) {
	const apiKey = "known-abuser"
	s := spawnService(t, abuseFlags()...)
	s.seedLimit(apiKey, 1)

	hammer(s, map[string]string{"X-Api-Key": apiKey}, 3)

	// Let several cleanup cycles run over the (still active, then
	// inactive) KnownCounter.
	s.waitCleanupRuns(6, 10*time.Second)

	if keys := s.keys(2, "rate:abuse:key:*"); len(keys) != 0 {
		t.Errorf("redisDB2 = %v, want empty — KnownCounters are never transferred", keys)
	}
	if keys := s.keys(3, "rate:abuse:ip:*"); len(keys) != 0 {
		t.Errorf("redisDB3 = %v, want empty", keys)
	}
	// Sanity check that the counter really was abusing — otherwise the
	// assertion above would pass for the wrong reason.
	if v := s.metric("ratelimit_requests_total", map[string]string{"result": "blocked_individual"}); v < 1 {
		t.Errorf("blocked_individual = %v, want ≥ 1 (the key should have been blocked)", v)
	}
}

// TestAbuse_TTLSlidesOnReupsert — every upsert re-arms the TTL, so a
// counter that keeps abusing keeps its redisDB3 record alive.
// miniredis TTLs only advance when the test fast-forwards its clock,
// which is exactly the control needed here.
func TestAbuse_TTLSlidesOnReupsert(t *testing.T) {
	const ip = "203.0.113.77"
	s := spawnService(t, abuseFlags()...)

	hammer(s, map[string]string{"X-Real-IP": ip}, 3)
	key := "rate:abuse:ip:" + ip
	waitFor(t, 6*time.Second, "abusive IP to reach redisDB3", func() bool {
		_, ok := s.hgetAll(3, key)
		return ok
	})

	s.redis.FastForward(6 * time.Second)
	shortened := s.ttl(3, key)
	if shortened > 5*time.Second {
		t.Fatalf("TTL after fast-forward = %v, want ≤ 5s", shortened)
	}

	// Keep abusing: the next cleanup cycle re-upserts and resets the TTL.
	hammer(s, map[string]string{"X-Real-IP": ip}, 2)
	waitFor(t, 6*time.Second, "TTL to slide back up", func() bool {
		return s.ttl(3, key) > shortened
	})
	if ttl := s.ttl(3, key); ttl <= 8*time.Second {
		t.Errorf("TTL after re-upsert = %v, want ≈ 10s", ttl)
	}
}

// TestAbuse_CleanupSurvivesRedisDown — with Redis unreachable the
// cleanup loop keeps running and keeps GC-ing memory; only the Redis
// writes are skipped (invariant 9).
func TestAbuse_CleanupSurvivesRedisDown(t *testing.T) {
	const ip = "203.0.113.99"
	rdb := newCtlRedis(t)
	s := spawnService(t, append(abuseFlags(), withRedisAddr(rdb.addr))...)

	hammer(s, map[string]string{"X-Real-IP": ip}, 2)
	rdb.stop()

	runsBefore := s.metric("ratelimit_cleanup_runs_total", nil)

	// The admin abuse pages answer 502 while Redis is unreachable (they
	// are a view over redisDB2/redisDB3), so observe the in-memory state
	// through the gauge cleanup publishes instead.
	if got := s.metric("ratelimit_counters_unknown_active", nil); got < 1 {
		t.Fatalf("counters_unknown_active = %v before the outage, want ≥ 1", got)
	}

	// No more traffic: the counter goes inactive and must be GC'd even
	// though its abuse record cannot be written anywhere.
	waitFor(t, 10*time.Second, "memory GC to continue with Redis down", func() bool {
		return s.metric("ratelimit_counters_unknown_active", nil) == 0
	})

	runsAfter := s.metric("ratelimit_cleanup_runs_total", nil)
	if runsAfter-runsBefore < 2 {
		t.Errorf("cleanup runs delta = %v, want ≥ 2 — the loop should keep ticking with Redis down",
			runsAfter-runsBefore)
	}
	// And the service is still serving.
	if code := s.mustCheck(map[string]string{"X-Real-IP": "203.0.113.100"}); code != 200 {
		t.Errorf("/check after Redis outage: status = %d, want 200", code)
	}
}
