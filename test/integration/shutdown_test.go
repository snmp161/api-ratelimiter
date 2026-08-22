// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Covers §17.2 H — graceful shutdown.

// TestShutdown_ExitsPromptly — SIGTERM and SIGINT both bring the
// process down well inside the 10s shutdown budget, with a clean exit
// code and the closing log line.
func TestShutdown_ExitsPromptly(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			s := spawnService(t)
			s.mustCheck(map[string]string{"X-Real-IP": "192.0.2.50"})

			took := s.signalAndWait(sig, 10*time.Second)
			if code := s.exitCode(); code != 0 {
				t.Errorf("exit code = %d, want 0\n--- stderr ---\n%s", code, s.stderrSnapshot())
			}
			if !strings.Contains(s.stdoutSnapshot(), "api-ratelimiter stopped") {
				t.Errorf("no shutdown log line\n--- stdout ---\n%s", s.stdoutSnapshot())
			}
			t.Logf("%v → exit in %v", sig, took)
		})
	}
}

// TestShutdown_FinalCleanupFlushesAbuse — the shutdown path runs one
// last cleanup, so abusive counters that never met a periodic cycle
// still land in Redis. --cleanup-interval=1h guarantees the only cycle
// that can write is the final one.
func TestShutdown_FinalCleanupFlushesAbuse(t *testing.T) {
	const ip = "203.0.113.200"
	s := spawnService(t,
		withFlag("--window", "second"),
		withFlag("--global-limit", "2"),
		withFlag("--burst", "1"),
		withFlag("--abuse-multiplier", "3"),
		withFlag("--abuse-transfer-threshold", "2"),
		withFlag("--cleanup-interval", "1h"),
		withFlag("--abuse-ttl", "10s"),
		withFlag("--log-level", "info"),
	)

	sent := hammer(s, map[string]string{"X-Real-IP": ip}, 3)

	key := "rate:abuse:ip:" + ip
	if _, ok := s.hgetAll(3, key); ok {
		t.Fatalf("%s written before shutdown — --cleanup-interval=1h should have prevented that", key)
	}

	s.signalAndWait(syscall.SIGTERM, 10*time.Second)

	vals, ok := s.hgetAll(3, key)
	if !ok {
		t.Fatalf("%s missing from redisDB3 — the final cleanup did not flush\n--- stdout ---\n%s",
			key, s.stdoutSnapshot())
	}
	if got := hfieldInt(t, vals, "total_requests"); got != int64(sent) {
		t.Errorf("total_requests = %d, want %d", got, sent)
	}
	if got := hfieldInt(t, vals, "abuse_hits"); got < 2 {
		t.Errorf("abuse_hits = %d, want ≥ 2", got)
	}
}

// TestShutdown_DrainsListeners — once SIGTERM lands the service stops
// reporting ready and stops serving /check.
//
// Note on what is asserted: main() flips /readyz to 503 first and then
// shuts the admin listener down in parallel with the rest of the
// drain, so the 503 window can be shorter than a poll interval and a
// prober may see "connection refused" instead. Both are correct
// "not ready" answers; what must never happen is a 200 after the
// service has started refusing. So the test asserts monotonicity —
// no OK after the first not-OK — plus a hard check that /check is
// unreachable once the process is gone.
func TestShutdown_DrainsListeners(t *testing.T) {
	s := spawnService(t)
	if code := s.mustCheck(map[string]string{"X-Real-IP": "192.0.2.51"}); code != http.StatusOK {
		t.Fatalf("pre-shutdown /check: status = %d, want 200", code)
	}

	type sample struct {
		ok  bool
		at  time.Time
		why string
	}
	var (
		mu      sync.Mutex
		samples []sample
		stop    = make(chan struct{})
		done    = make(chan struct{})
	)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			resp, err := client.Get("http://" + s.adminAddr + "/readyz")
			smp := sample{at: time.Now()}
			if err != nil {
				smp.why = err.Error()
			} else {
				smp.ok = resp.StatusCode == http.StatusOK
				smp.why = resp.Status
				resp.Body.Close()
			}
			mu.Lock()
			samples = append(samples, smp)
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
		}
	}()

	// Let the prober collect a few healthy samples first.
	time.Sleep(50 * time.Millisecond)
	s.signalAndWait(syscall.SIGTERM, 10*time.Second)
	close(stop)
	<-done

	mu.Lock()
	defer mu.Unlock()
	firstBad := -1
	for i, smp := range samples {
		if !smp.ok && firstBad < 0 {
			firstBad = i
		}
		if firstBad >= 0 && smp.ok {
			t.Errorf("/readyz answered OK at %v after it had already stopped being ready at %v (%q)",
				smp.at, samples[firstBad].at, samples[firstBad].why)
			break
		}
	}
	if firstBad < 0 {
		t.Errorf("/readyz never stopped answering 200 during shutdown (%d samples)", len(samples))
	} else {
		t.Logf("readiness dropped after %d OK samples: %s", firstBad, samples[firstBad].why)
	}

	// The /check listener is gone for good.
	if _, err := s.check(map[string]string{"X-Real-IP": "192.0.2.52"}); err == nil {
		t.Errorf("/check still answers after the process exited")
	}
	d := net.Dialer{Timeout: time.Second}
	if conn, err := d.DialContext(context.Background(), "unix", s.socketPath); err == nil {
		conn.Close()
		t.Errorf("unix socket %s still accepts connections after exit", s.socketPath)
	}
}
