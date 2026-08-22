// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Covers §17.2 K — regression armor for bugs this project has already
// had once. Each test names the invariant it protects.

// TestEdge_RedisDownDoesNotFloodTheLog — invariant 9: Redis health is
// tracked centrally and only transitions are logged. A burst of
// requests during an outage must produce at most one "redis unhealthy"
// warning, not one per request.
func TestEdge_RedisDownDoesNotFloodTheLog(t *testing.T) {
	rdb := newCtlRedis(t)
	s := spawnService(t,
		withRedisAddr(rdb.addr),
		withFlag("--global-limit", "1000"),
		withFlag("--window", "minute"),
		withFlag("--cleanup-interval", "1h"), // keep cleanup out of the log
		withFlag("--log-level", "info"),
	)
	// Touch Redis once while it is up so the health state starts healthy.
	s.mustCheck(map[string]string{"X-Api-Key": "warmup"})

	rdb.stop()

	const n = 50
	codes := s.checkN(n, map[string]string{"X-Api-Key": "flood-key"})
	if got := countStatus(codes, http.StatusOK); got != n {
		t.Errorf("during the outage: 200s = %d/%d, want all (fail open)", got, n)
	}

	if got := strings.Count(s.stdoutSnapshot(), "redis unhealthy"); got > 1 {
		t.Errorf("%d \"redis unhealthy\" lines for %d requests, want ≤ 1 (transition-only logging)", got, n)
	}

	// …and recovery is logged once Redis is back.
	rdb.start()
	waitFor(t, 10*time.Second, "redis recovery to be logged", func() bool {
		s.mustCheck(map[string]string{"X-Api-Key": "recovery-probe"})
		return strings.Contains(s.stdoutSnapshot(), "redis recovered")
	})
}

// TestEdge_CleanupDoesNotDropActiveCounter — invariants 6 and 7: a
// counter that keeps receiving trickle traffic must survive every
// cleanup cycle. If Snapshot→Delete were not atomic, or if the
// inactivity threshold were one window instead of two, the counter
// would be recreated from zero and Total would fall behind.
func TestEdge_CleanupDoesNotDropActiveCounter(t *testing.T) {
	const ip = "203.0.113.30"
	s := spawnService(t,
		withFlag("--window", "second"),
		withFlag("--global-limit", "100"),
		withFlag("--cleanup-interval", "200ms"), // many cycles during the trickle
		withFlag("--log-level", "info"),
	)

	// One request every 300ms — that is more than one window apart, so
	// only the 2×window rule keeps the counter alive.
	const requests = 12
	for i := 0; i < requests; i++ {
		s.mustCheck(map[string]string{"X-Real-IP": ip})
		time.Sleep(300 * time.Millisecond)
	}

	if got := s.topTotal("/abuse/ips", ip); got != requests {
		t.Errorf("counter total = %d, want %d — the counter was dropped and recreated mid-trickle", got, requests)
	}
}

// TestEdge_InactivityThresholdIsTwoWindows — invariant 7 from the other
// side: a pause shorter than 2×window keeps the counter, a longer one
// lets cleanup collect it.
func TestEdge_InactivityThresholdIsTwoWindows(t *testing.T) {
	const ip = "203.0.113.31"
	s := spawnService(t,
		withFlag("--window", "second"),
		withFlag("--global-limit", "100"),
		withFlag("--cleanup-interval", "200ms"),
		withFlag("--log-level", "info"),
	)

	s.mustCheck(map[string]string{"X-Real-IP": ip})
	time.Sleep(1300 * time.Millisecond) // > 1 window, < 2 windows
	s.mustCheck(map[string]string{"X-Real-IP": ip})
	if got := s.topTotal("/abuse/ips", ip); got != 2 {
		t.Errorf("total after a 1.3s pause = %d, want 2 — the counter must survive a sub-2×window gap", got)
	}

	// Now go quiet for longer than 2×window and let cleanup run.
	waitFor(t, 10*time.Second, "counter to be collected after 2×window of silence", func() bool {
		return s.topTotal("/abuse/ips", ip) == -1
	})

	s.mustCheck(map[string]string{"X-Real-IP": ip})
	if got := s.topTotal("/abuse/ips", ip); got != 1 {
		t.Errorf("total after recreation = %d, want 1 (a fresh counter)", got)
	}
}

// TestEdge_OriginalURIWithoutQueryFallsBackToIP — an X-Original-URI
// with no api_key/token must not become a counter key of its own; the
// request is limited by IP.
func TestEdge_OriginalURIWithoutQueryFallsBackToIP(t *testing.T) {
	const ip = "203.0.113.32"
	s := spawnService(t,
		withFlag("--global-limit", "1"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
	)

	headers := map[string]string{
		"X-Original-URI": "/api/v1/健康", // no query at all
		"X-Real-IP":      ip,
	}
	if code := s.mustCheck(headers); code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", code)
	}
	if code := s.mustCheck(headers); code != http.StatusForbidden {
		t.Errorf("second request: status = %d, want 403 — the IP counter should apply", code)
	}

	if keys := s.topKeys("/abuse/ips"); !containsStr(keys, ip) {
		t.Errorf("in-memory IP counters = %v, want to contain %s", keys, ip)
	}
	if keys := s.topKeys("/abuse/keys"); len(keys) != 0 {
		t.Errorf("in-memory key counters = %v, want none — no api_key was present", keys)
	}
}

// TestEdge_PanicSafetyNetKeepsCheckAlive — invariant 4: /check is
// wrapped in recover() so nginx never sees a 5xx. There is no way to
// force a panic from outside, so this test settles for the observable
// contract: a long, hostile mix of requests never produces a 5xx and
// never kills the process.
func TestEdge_PanicSafetyNetKeepsCheckAlive(t *testing.T) {
	s := spawnService(t,
		withFlag("--global-limit", "3"),
		withFlag("--window", "minute"),
		withFlag("--log-level", "info"),
	)

	hostile := []map[string]string{
		{"X-Api-Key": strings.Repeat("x", 8192)},
		{"X-Real-IP": strings.Repeat("1.2.3.4,", 500)},
		{"X-Original-URI": "/?api_key=" + strings.Repeat("y", 8192)},
		{"X-Api-Key": "\x80\xfe-raw-bytes"}, // valid header bytes, invalid UTF-8
		{"X-Original-URI": "?api_key=%"},
		{"X-Api-Key": "", "X-Real-IP": ""},
	}
	for i, h := range hostile {
		for j := 0; j < 5; j++ {
			code, body, err := s.doCheck(http.MethodGet, "/check", h)
			if err != nil {
				t.Fatalf("case %d: %v", i, err)
			}
			if code >= 500 {
				t.Errorf("case %d: status = %d — nginx would turn this into a 500", i, code)
			}
			if len(body) != 0 {
				t.Errorf("case %d: body = %q, want empty", i, body)
			}
		}
	}

	if code, _ := s.adminBody("/healthz"); code != http.StatusOK {
		t.Errorf("/healthz after hostile traffic: status = %d, want 200", code)
	}
}
