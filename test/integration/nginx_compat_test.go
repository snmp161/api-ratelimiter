// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"net/http"
	"strings"
	"testing"
)

// Covers §17.2 J — the request shapes nginx/Angie actually produces
// through auth_request, and the contract that /check answers with
// exactly 200 or 403 and an empty body (invariant 1).

// TestNginxCompat_AuthRequestShape — auth_request issues the subrequest
// with the parent's method and, with proxy_pass_request_body off, no
// body. X-Original-URI carries the real request URI.
func TestNginxCompat_AuthRequestShape(t *testing.T) {
	s := spawnService(t,
		withFlag("--global-limit", "1"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
	)

	headers := map[string]string{
		"X-Original-URI": "/api/v1/orders?api_key=nginx-key&page=2",
		"X-Real-IP":      "203.0.113.11",
		"Host":           "api.example.com",
		"User-Agent":     "",
	}

	code, body, err := s.doCheck(http.MethodPost, "/check", headers)
	if err != nil {
		t.Fatalf("POST /check: %v", err)
	}
	if code != http.StatusOK {
		t.Errorf("first POST /check: status = %d, want 200", code)
	}
	if len(body) != 0 {
		t.Errorf("response body = %q, want empty", body)
	}

	// Second request exceeds the global limit of 1 → 403, still empty.
	code, body, err = s.doCheck(http.MethodPost, "/check", headers)
	if err != nil {
		t.Fatalf("POST /check: %v", err)
	}
	if code != http.StatusForbidden {
		t.Errorf("second POST /check: status = %d, want 403 (429 would break auth_request)", code)
	}
	if len(body) != 0 {
		t.Errorf("blocked response body = %q, want empty", body)
	}
}

// TestNginxCompat_OnlyAllowOrBlock — whatever nginx (or a confused
// client) throws at /check, the answer must stay inside {200, 403}
// with an empty body. Anything else turns into a 500 for the parent
// request.
func TestNginxCompat_OnlyAllowOrBlock(t *testing.T) {
	s := spawnService(t,
		withFlag("--global-limit", "2"),
		withFlag("--burst", "0"),
		withFlag("--window", "minute"),
	)

	longKey := strings.Repeat("k", 4096)
	cases := []struct {
		name    string
		method  string
		target  string
		headers map[string]string
	}{
		{"GET no headers", http.MethodGet, "/check", nil},
		{"POST no headers", http.MethodPost, "/check", nil},
		{"HEAD", http.MethodHead, "/check", map[string]string{"X-Real-IP": "203.0.113.12"}},
		{"PUT", http.MethodPut, "/check", map[string]string{"X-Api-Key": "put-key"}},
		{"DELETE", http.MethodDelete, "/check", map[string]string{"X-Api-Key": "del-key"}},
		{"empty X-Original-URI", http.MethodGet, "/check", map[string]string{"X-Original-URI": ""}},
		{"X-Original-URI without query", http.MethodGet, "/check",
			map[string]string{"X-Original-URI": "/api/v1/ping", "X-Real-IP": "203.0.113.13"}},
		{"unparseable X-Original-URI", http.MethodGet, "/check",
			map[string]string{"X-Original-URI": "://%%%", "X-Real-IP": "203.0.113.14"}},
		{"garbage X-Real-IP", http.MethodGet, "/check", map[string]string{"X-Real-IP": "%%%%"}},
		{"unicode api_key", http.MethodGet, "/check", map[string]string{"X-Api-Key": "ключ-🔑"}},
		{"very long api_key", http.MethodGet, "/check", map[string]string{"X-Api-Key": longKey}},
		{"query junk", http.MethodGet, "/check?api_key=&token=&x=%zz", nil},
		{"both key and ip", http.MethodGet, "/check?api_key=q",
			map[string]string{"X-Api-Key": "h", "X-Real-IP": "203.0.113.15"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Repeat each case enough times to also cross the limit —
			// blocked responses must obey the same contract.
			for i := 0; i < 4; i++ {
				code, body, err := s.doCheck(tc.method, tc.target, tc.headers)
				if err != nil {
					t.Fatalf("request %d: %v", i+1, err)
				}
				if code != http.StatusOK && code != http.StatusForbidden {
					t.Fatalf("request %d: status = %d, want 200 or 403", i+1, code)
				}
				if len(body) != 0 {
					t.Errorf("request %d: body = %q, want empty", i+1, body)
				}
			}
		})
	}
}

// TestNginxCompat_UnknownPathIsNotProxied — only /check is served on
// the auth_request listener. A stray path returns 404, which is a
// configuration error on the nginx side rather than an allow.
func TestNginxCompat_UnknownPathIsNotProxied(t *testing.T) {
	s := spawnService(t)

	code, _, err := s.doCheck(http.MethodGet, "/checkk", nil)
	if err != nil {
		t.Fatalf("GET /checkk: %v", err)
	}
	if code != http.StatusNotFound {
		t.Errorf("GET /checkk: status = %d, want 404", code)
	}
}
