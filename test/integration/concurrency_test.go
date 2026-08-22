// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Covers §17.2 I — behaviour under concurrent load.

// TestConcurrency_SingleKeyExactAccounting — 100 goroutines × 10
// requests against one api_key with an individual limit of 200. The
// counter is mutex-protected and --window=minute keeps everything in
// one slot, so the split is not merely "about right": exactly `limit`
// requests are allowed and the rest are blocked.
func TestConcurrency_SingleKeyExactAccounting(t *testing.T) {
	const (
		goroutines = 100
		perRoutine = 10
		total      = goroutines * perRoutine
		limit      = 200
	)
	s := spawnService(t,
		withFlag("--global-limit", "1000"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
		withFlag("--log-level", "info"), // a debug line per request would dominate the run
	)
	s.seedLimit("hot-key", limit)

	// Warm one connection so the individual limit is already in the
	// counter map before the storm starts.
	s.mustCheck(map[string]string{"X-Api-Key": "hot-key"})

	start := time.Now()
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
		blocked int
		other   []int
		errs    []error
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perRoutine; j++ {
				code, _, err := s.doCheck(http.MethodGet, "/check", map[string]string{"X-Api-Key": "hot-key"})
				mu.Lock()
				switch {
				case err != nil:
					errs = append(errs, err)
				case code == http.StatusOK:
					allowed++
				case code == http.StatusForbidden:
					blocked++
				default:
					other = append(other, code)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	crossed := !sameWindow(start, time.Now(), 60)

	if len(errs) > 0 {
		t.Fatalf("%d requests failed, first: %v", len(errs), errs[0])
	}
	if len(other) > 0 {
		t.Errorf("unexpected statuses (only 200/403 are allowed): %v", other)
	}
	if allowed+blocked != total {
		t.Errorf("allowed+blocked = %d, want %d", allowed+blocked, total)
	}

	// The warm-up request already consumed one slot of the limit.
	wantAllowed := limit - 1
	switch {
	case crossed:
		// A minute boundary opened a fresh window mid-run; the counter
		// legitimately allowed another batch, so only the lower bound
		// holds.
		t.Logf("crossed a minute boundary: allowed=%d blocked=%d", allowed, blocked)
		if allowed < wantAllowed {
			t.Errorf("allowed = %d, want at least %d", allowed, wantAllowed)
		}
	case allowed != wantAllowed:
		t.Errorf("allowed = %d, want exactly %d (limit %d minus the warm-up request)",
			allowed, wantAllowed, limit)
	}

	// The service's own accounting must agree with what the clients saw.
	if got := s.metric("ratelimit_requests_total", map[string]string{"result": "allowed"}); got != float64(allowed+1) {
		t.Errorf("metric allowed = %v, want %d (client-observed + warm-up)", got, allowed+1)
	}
	if got := s.metric("ratelimit_requests_total", map[string]string{"result": "blocked_individual"}); got != float64(blocked) {
		t.Errorf("metric blocked_individual = %v, want %d", got, blocked)
	}
}

// TestConcurrency_AdminActionsUnderTraffic — admin deletes and purges
// firing in parallel with live /check traffic must not corrupt shared
// state or take the service down.
//
// The workers use bare HTTP calls rather than the t.Fatalf-ing helpers:
// failing a test from a non-test goroutine is not allowed, and here a
// transient error is not what is under test anyway — the assertions
// come after the storm.
func TestConcurrency_AdminActionsUnderTraffic(t *testing.T) {
	s := spawnService(t,
		withFlag("--global-limit", "1000"),
		withFlag("--window", "minute"),
		withFlag("--cleanup-interval", "200ms"),
		withFlag("--log-level", "info"),
	)
	const n = 20
	for i := 0; i < n; i++ {
		s.seedLimit(fmt.Sprintf("limit-%02d", i), 100)
		s.seedAbuseKey(fmt.Sprintf("abuse-%02d", i))
		s.seedAbuseIP(fmt.Sprintf("198.51.100.%d", i+1))
	}

	token := s.csrfToken("/limits")
	client := adminClient()
	get := func(path string) {
		resp, err := client.Get("http://" + s.adminAddr + path)
		if err != nil {
			return
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		resp.Body.Close()
	}
	post := func(path string, form url.Values) {
		resp, err := client.PostForm("http://"+s.adminAddr+path, form)
		if err != nil {
			return
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		resp.Body.Close()
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Traffic against both counter maps.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _, _ = s.doCheck(http.MethodGet, "/check", map[string]string{"X-Api-Key": fmt.Sprintf("limit-%02d", i)})
				_, _, _ = s.doCheck(http.MethodGet, "/check", map[string]string{"X-Real-IP": fmt.Sprintf("198.51.100.%d", i+1)})
			}
		}(i)
	}

	// Admin readers and writers, each doing a fixed number of rounds.
	actions := []func(){
		func() { get("/") },
		func() { get("/limits") },
		func() { get("/abuse/keys") },
		func() { get("/abuse/ips") },
		func() {
			post("/limits/delete", url.Values{"csrf_token": {token}, "keys": {"limit-01"}})
		},
		func() {
			post("/abuse/keys/delete", url.Values{"csrf_token": {token}, "keys": {"abuse-02"}})
		},
		func() {
			post("/abuse/ips/purge", url.Values{"csrf_token": {token}, "confirm": {"yes"}})
		},
	}
	for _, fn := range actions {
		wg.Add(1)
		go func(fn func()) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				fn()
			}
		}(fn)
	}

	time.Sleep(2 * time.Second)
	close(stop)
	wg.Wait()

	// Still alive, still serving, and nothing panicked along the way.
	if code, _ := s.adminBody("/healthz"); code != http.StatusOK {
		t.Errorf("/healthz after the storm: status = %d, want 200", code)
	}
	if code := s.mustCheck(map[string]string{"X-Real-IP": "192.0.2.99"}); code != http.StatusOK {
		t.Errorf("/check after the storm: status = %d, want 200", code)
	}
	if out := s.stdoutSnapshot(); strings.Contains(out, "panic") {
		t.Errorf("panic recovered during concurrent admin actions:\n%s", out)
	}
	// The writes really happened — the service was not simply ignoring them.
	if _, ok := s.hgetAll(1, "rate:limit:limit-01"); ok {
		t.Errorf("rate:limit:limit-01 survived the concurrent delete")
	}
	if _, ok := s.hgetAll(2, "rate:abuse:key:abuse-02"); ok {
		t.Errorf("rate:abuse:key:abuse-02 survived the concurrent delete")
	}
	if keys := s.keys(3, "rate:abuse:ip:*"); len(keys) != 0 {
		t.Errorf("redisDB3 = %v, want empty after the concurrent purge", keys)
	}
}
