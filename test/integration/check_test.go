// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Covers §17.2 B — /check rate-limiting basics and the api_key
// extraction priority chain.
//
// Window choice: tests that must fit all their requests into a single
// slot run with --window=minute, so a second-boundary crossing mid-test
// can't split the counter. Tests that specifically exercise the slot
// change use --window=second and align themselves to a boundary first.

// TestCheck_NoKeyNoIP_AlwaysAllowed — with neither an api_key nor a
// usable X-Real-IP there is nothing to limit by, so every request
// passes regardless of the global limit (invariant 2, "fail open
// without key").
func TestCheck_NoKeyNoIP_AlwaysAllowed(t *testing.T) {
	s := spawnService(t,
		withFlag("--global-limit", "2"),
		withFlag("--window", "minute"),
	)

	codes := s.checkN(10, nil)
	if got := countStatus(codes, http.StatusOK); got != 10 {
		t.Errorf("keyless requests: 200s = %d/10, want 10 (statuses: %v)", got, codes)
	}
}

// TestCheck_IPGlobalLimit — without an api_key the IP is the counter
// key and --global-limit applies: N allowed, N+1 blocked.
func TestCheck_IPGlobalLimit(t *testing.T) {
	const limit = 5
	s := spawnService(t,
		withFlag("--global-limit", "5"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
	)

	start := time.Now()
	codes := s.checkN(limit+1, map[string]string{"X-Real-IP": "10.0.0.7"})
	if !sameWindow(start, time.Now(), 60) {
		t.Skip("requests straddled a minute boundary — counter reset mid-test")
	}

	for i := 0; i < limit; i++ {
		if codes[i] != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i+1, codes[i])
		}
	}
	if codes[limit] != http.StatusForbidden {
		t.Errorf("request %d (over limit): status = %d, want 403", limit+1, codes[limit])
	}
}

// TestCheck_InvalidRealIPIgnored — a malformed X-Real-IP must be
// treated as absent (invariant 5). With no api_key that means "nothing
// to limit by" → every request allowed, no matter how low the limit.
func TestCheck_InvalidRealIPIgnored(t *testing.T) {
	s := spawnService(t,
		withFlag("--global-limit", "1"),
		withFlag("--window", "minute"),
	)

	bogus := []string{
		"not-an-ip",
		"999.999.999.999",
		"1.2.3.4, 5.6.7.8", // X-Forwarded-For style list, not a bare IP
		"1.2.3.4:8080",     // host:port
		"::gg",
	}
	for _, v := range bogus {
		t.Run(v, func(t *testing.T) {
			codes := s.checkN(5, map[string]string{"X-Real-IP": v})
			if got := countStatus(codes, http.StatusOK); got != 5 {
				t.Errorf("X-Real-IP %q: 200s = %d/5, want 5 (should be ignored) — statuses: %v",
					v, got, codes)
			}
		})
	}
}

// TestCheck_IPv6Accepted — valid IPv6 literals are real counter keys.
func TestCheck_IPv6Accepted(t *testing.T) {
	s := spawnService(t,
		withFlag("--global-limit", "2"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
	)

	for _, ip := range []string{"::1", "2001:db8::1"} {
		t.Run(ip, func(t *testing.T) {
			start := time.Now()
			codes := s.checkN(3, map[string]string{"X-Real-IP": ip})
			if !sameWindow(start, time.Now(), 60) {
				t.Skip("requests straddled a minute boundary")
			}
			if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
				t.Errorf("%s: first two requests = %v, want 200 200", ip, codes[:2])
			}
			if codes[2] != http.StatusForbidden {
				t.Errorf("%s: third request = %d, want 403", ip, codes[2])
			}
		})
	}
}

// TestCheck_UnknownAPIKeyUsesGlobalLimit — an api_key absent from
// redisDB1 lands in UnknownCounters under the "key:" prefix and gets
// the global limit; blocking is counted as blocked_global.
func TestCheck_UnknownAPIKeyUsesGlobalLimit(t *testing.T) {
	const limit = 3
	s := spawnService(t,
		withFlag("--global-limit", "3"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
	)

	start := time.Now()
	codes := s.checkN(limit+1, map[string]string{"X-Api-Key": "not-in-db1"})
	if !sameWindow(start, time.Now(), 60) {
		t.Skip("requests straddled a minute boundary")
	}
	if got := countStatus(codes, http.StatusOK); got != limit {
		t.Errorf("200s = %d, want %d (statuses: %v)", got, limit, codes)
	}
	if codes[limit] != http.StatusForbidden {
		t.Errorf("request over limit: status = %d, want 403", codes[limit])
	}

	if v := s.metric("ratelimit_requests_total", map[string]string{"result": "blocked_global"}); v != 1 {
		t.Errorf("blocked_global = %v, want 1", v)
	}
	if v := s.metric("ratelimit_requests_total", map[string]string{"result": "blocked_individual"}); v != 0 {
		t.Errorf("blocked_individual = %v, want 0 (key is not in redisDB1)", v)
	}

	// The counter is stored with the "key:" prefix, so it shows up in
	// the UnknownCounters top table on /abuse/keys — and must NOT show
	// up on /abuse/ips, which filters on "ip:".
	_, keysPage := s.adminBody("/abuse/keys")
	if !strings.Contains(keysPage, "not-in-db1") {
		t.Errorf("/abuse/keys top table does not mention the api_key")
	}
	_, ipsPage := s.adminBody("/abuse/ips")
	if strings.Contains(ipsPage, "not-in-db1") {
		t.Errorf("/abuse/ips top table leaked a key: counter")
	}
}

// TestCheck_KeyAndIPCountersIndependent — "1.2.3.4" as an api_key and
// "1.2.3.4" as an IP are different counters (prefix non-collision).
func TestCheck_KeyAndIPCountersIndependent(t *testing.T) {
	const same = "1.2.3.4"
	s := spawnService(t,
		withFlag("--global-limit", "2"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
	)

	start := time.Now()
	byKey := s.checkN(3, map[string]string{"X-Api-Key": same})
	if byKey[2] != http.StatusForbidden {
		t.Fatalf("api_key counter not exhausted: %v", byKey)
	}
	// Same string, now as an IP: a fresh counter, so it must pass.
	byIP := s.checkN(2, map[string]string{"X-Real-IP": same})
	if !sameWindow(start, time.Now(), 60) {
		t.Skip("requests straddled a minute boundary")
	}
	if got := countStatus(byIP, http.StatusOK); got != 2 {
		t.Errorf("ip counter statuses = %v, want two 200s — counters collided with the key: one", byIP)
	}
}

// TestCheck_BurstZone — requests above the limit but within burst are
// allowed; past limit+burst they are blocked.
func TestCheck_BurstZone(t *testing.T) {
	const (
		limit = 10
		burst = 5
	)
	s := spawnService(t,
		withFlag("--global-limit", "10"),
		withFlag("--burst", "5"),
		withFlag("--window", "minute"),
	)

	start := time.Now()
	codes := s.checkN(limit+burst+2, map[string]string{"X-Real-IP": "10.0.0.8"})
	if !sameWindow(start, time.Now(), 60) {
		t.Skip("requests straddled a minute boundary")
	}

	for i := 0; i < limit+burst; i++ {
		if codes[i] != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200 (limit=%d burst=%d)", i+1, codes[i], limit, burst)
		}
	}
	for i := limit + burst; i < len(codes); i++ {
		if codes[i] != http.StatusForbidden {
			t.Errorf("request %d: status = %d, want 403 (past limit+burst)", i+1, codes[i])
		}
	}
}

// TestCheck_WindowRollover — an exhausted counter starts a new slot at
// the next second and allows requests again.
func TestCheck_WindowRollover(t *testing.T) {
	s := spawnService(t,
		withFlag("--global-limit", "2"),
		withFlag("--burst", "0"),
		withFlag("--window", "second"),
	)

	// Land at the very beginning of a second so all three requests are
	// guaranteed to share a slot.
	sleepToNextSecond(50 * time.Millisecond)
	codes := s.checkN(3, map[string]string{"X-Real-IP": "10.0.0.9"})
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK || codes[2] != http.StatusForbidden {
		t.Fatalf("in-window statuses = %v, want [200 200 403]", codes)
	}

	sleepToNextSecond(50 * time.Millisecond)
	if code := s.mustCheck(map[string]string{"X-Real-IP": "10.0.0.9"}); code != http.StatusOK {
		t.Errorf("after slot change: status = %d, want 200", code)
	}
}

// TestCheck_MinuteWindowSurvivesSecondTick — with --window=minute the
// slot must not reset every second. Guards against a regression to
// hardcoded 1s slots.
func TestCheck_MinuteWindowSurvivesSecondTick(t *testing.T) {
	s := spawnService(t,
		withFlag("--global-limit", "1"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
	)

	start := time.Now()
	if code := s.mustCheck(map[string]string{"X-Real-IP": "10.0.0.10"}); code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", code)
	}
	if code := s.mustCheck(map[string]string{"X-Real-IP": "10.0.0.10"}); code != http.StatusForbidden {
		t.Fatalf("second request: status = %d, want 403", code)
	}

	time.Sleep(1200 * time.Millisecond) // more than a second, far less than a minute
	code := s.mustCheck(map[string]string{"X-Real-IP": "10.0.0.10"})
	if !sameWindow(start, time.Now(), 60) {
		t.Skip("test straddled a minute boundary — the slot legitimately reset")
	}
	if code != http.StatusForbidden {
		t.Errorf("after 1.2s with --window=minute: status = %d, want 403 (same slot)", code)
	}
}

// TestCheck_KeyExtractionPriority — the api_key is looked up in three
// places (header → query → X-Original-URI), and inside X-Original-URI
// api_key wins over token.
//
// How each case proves which source won: only the expected winner has a
// limit of 1 in redisDB1, every decoy is absent from redisDB1 and
// therefore gets the global limit of 100. So a 403 on the second
// request can only come from the expected key's individual limit — and
// blocked_individual (not blocked_global) must be the metric that
// moves.
func TestCheck_KeyExtractionPriority(t *testing.T) {
	s := spawnService(t,
		withFlag("--global-limit", "100"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
	)

	for _, k := range []string{"hdr-key", "qry-key", "qry-token", "uri-key", "uri-token", "uri-both"} {
		s.seedLimit(k, 1)
	}

	cases := []struct {
		name    string
		target  string
		headers map[string]string
	}{
		{
			name:   "header beats query and X-Original-URI",
			target: "/check?api_key=qry-decoy",
			headers: map[string]string{
				"X-Api-Key":      "hdr-key",
				"X-Original-URI": "/v1/data?api_key=uri-decoy",
			},
		},
		{
			name:    "query api_key beats X-Original-URI",
			target:  "/check?api_key=qry-key",
			headers: map[string]string{"X-Original-URI": "/v1/data?api_key=uri-decoy"},
		},
		{
			name:    "query token",
			target:  "/check?token=qry-token",
			headers: nil,
		},
		{
			name:    "X-Original-URI api_key",
			target:  "/check",
			headers: map[string]string{"X-Original-URI": "/v1/data?api_key=uri-key&foo=bar"},
		},
		{
			name:    "X-Original-URI token",
			target:  "/check",
			headers: map[string]string{"X-Original-URI": "/v1/data?token=uri-token"},
		},
		{
			name:    "X-Original-URI api_key beats token",
			target:  "/check",
			headers: map[string]string{"X-Original-URI": "/v1/data?token=uri-decoy&api_key=uri-both"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := s.metric("ratelimit_requests_total", map[string]string{"result": "blocked_individual"})

			first, _, err := s.doCheck(http.MethodGet, tc.target, tc.headers)
			if err != nil {
				t.Fatalf("first request: %v", err)
			}
			second, _, err := s.doCheck(http.MethodGet, tc.target, tc.headers)
			if err != nil {
				t.Fatalf("second request: %v", err)
			}

			if first != http.StatusOK {
				t.Errorf("first request: status = %d, want 200", first)
			}
			if second != http.StatusForbidden {
				t.Errorf("second request: status = %d, want 403 — the expected api_key "+
					"(individual limit 1) was not the one extracted", second)
			}

			after := s.metric("ratelimit_requests_total", map[string]string{"result": "blocked_individual"})
			if after-before != 1 {
				t.Errorf("blocked_individual delta = %v, want 1 (block must come from the redisDB1 limit)",
					after-before)
			}
		})
	}
}
