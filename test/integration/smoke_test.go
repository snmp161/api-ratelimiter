// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestSmoke_StartsAndAnswersHealthz is the bare-minimum integration
// check: build → spawn → wait → exit. Covers part of §17.2 A "Сервис
// стартует с дефолтным конфигом". Each subsequent integration test
// implicitly relies on this path working — if it doesn't, every other
// test will fail the same way and this is the cleanest failure
// signal.
func TestSmoke_StartsAndAnswersHealthz(t *testing.T) {
	s := spawnService(t)

	// spawnService already polled /healthz to ready; re-check that
	// the response is in fact 200 and that /readyz also responds
	// (we have miniredis up, so it should be 200 too).
	resp, err := s.adminGET("/healthz")
	if err != nil {
		t.Fatalf("/healthz: %v", err)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}

	resp, err = s.adminGET("/readyz")
	if err != nil {
		t.Fatalf("/readyz: %v", err)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/readyz status = %d, want 200 (miniredis is reachable)", resp.StatusCode)
	}

	// /metrics returns the Prometheus exposition with our registered
	// metric names — proves the metrics listener is up and the
	// registry is wired.
	body, code, err := s.metricsGET()
	if err != nil {
		t.Fatalf("/metrics: %v", err)
	}
	if code != http.StatusOK {
		t.Errorf("/metrics status = %d, want 200", code)
	}
	if !strings.Contains(body, "ratelimit_requests_total") {
		t.Errorf("/metrics body missing ratelimit_requests_total")
	}
	if !strings.Contains(body, "ratelimit_check_duration_seconds") {
		t.Errorf("/metrics body missing ratelimit_check_duration_seconds histogram")
	}
}
