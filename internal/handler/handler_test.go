// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"ratelimiter/internal/metrics"
)

type spyDecider struct {
	gotKey, gotIP string
	allow         bool
	panicWith     any
}

func (s *spyDecider) Decide(_ context.Context, key, ip string) bool {
	s.gotKey = key
	s.gotIP = ip
	if s.panicWith != nil {
		panic(s.panicWith)
	}
	return s.allow
}

func newTestCheck(d Decider) *Check {
	return NewCheck(d, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestCheck_Allowed(t *testing.T) {
	d := &spyDecider{allow: true}
	h := newTestCheck(d)
	r := httptest.NewRequest("GET", "/check", nil)
	r.Header.Set("X-Api-Key", "abc")
	r.Header.Set("X-Real-IP", "1.1.1.1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d want 200", w.Code)
	}
	if d.gotKey != "abc" || d.gotIP != "1.1.1.1" {
		t.Fatalf("decider received key=%q ip=%q", d.gotKey, d.gotIP)
	}
}

func TestCheck_Blocked(t *testing.T) {
	d := &spyDecider{allow: false}
	h := newTestCheck(d)
	r := httptest.NewRequest("GET", "/check", nil)
	r.Header.Set("X-Api-Key", "abc")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d want 403 (auth_request only forwards 401/403 — see check.go)", w.Code)
	}
}

func TestCheck_EmptyApiKeyUsesIP(t *testing.T) {
	d := &spyDecider{allow: true}
	h := newTestCheck(d)
	r := httptest.NewRequest("GET", "/check", nil)
	r.Header.Set("X-Real-IP", "9.9.9.9")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if d.gotKey != "" || d.gotIP != "9.9.9.9" {
		t.Fatalf("decider got key=%q ip=%q want empty key, IP 9.9.9.9", d.gotKey, d.gotIP)
	}
}

func TestCheck_PanicFailsOpen(t *testing.T) {
	d := &spyDecider{panicWith: "boom"}
	h := newTestCheck(d)
	r := httptest.NewRequest("GET", "/check", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("panic must produce 200 (fail open), got %d", w.Code)
	}
}

func TestCheck_KeyFromQueryApiKey(t *testing.T) {
	d := &spyDecider{allow: true}
	h := newTestCheck(d)
	r := httptest.NewRequest("GET", "/check?api_key=qkey&country_id=5", nil)
	r.Header.Set("X-Real-IP", "1.1.1.1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if d.gotKey != "qkey" {
		t.Fatalf("decider got key=%q, want qkey", d.gotKey)
	}
}

func TestCheck_KeyFromQueryTokenFallback(t *testing.T) {
	d := &spyDecider{allow: true}
	h := newTestCheck(d)
	r := httptest.NewRequest("GET", "/check?token=qtok", nil)
	r.Header.Set("X-Real-IP", "1.1.1.1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if d.gotKey != "qtok" {
		t.Fatalf("decider got key=%q, want qtok", d.gotKey)
	}
}

func TestCheck_KeyFromOriginalURIHeader(t *testing.T) {
	d := &spyDecider{allow: true}
	h := newTestCheck(d)
	r := httptest.NewRequest("GET", "/check", nil)
	r.Header.Set("X-Original-URI", "/control/get-number?api_key=I3VNMQ&country_id=5")
	r.Header.Set("X-Real-IP", "1.1.1.1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if d.gotKey != "I3VNMQ" {
		t.Fatalf("decider got key=%q, want I3VNMQ", d.gotKey)
	}
}

func TestCheck_KeyPriorityHeaderOverQuery(t *testing.T) {
	d := &spyDecider{allow: true}
	h := newTestCheck(d)
	r := httptest.NewRequest("GET", "/check?api_key=q-value", nil)
	r.Header.Set("X-Api-Key", "header-value")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if d.gotKey != "header-value" {
		t.Fatalf("decider got key=%q, want header-value (X-Api-Key wins over query)", d.gotKey)
	}
}

func TestCheck_KeyPriorityQueryOverOriginalURI(t *testing.T) {
	d := &spyDecider{allow: true}
	h := newTestCheck(d)
	r := httptest.NewRequest("GET", "/check?api_key=qkey", nil)
	r.Header.Set("X-Original-URI", "/some?api_key=urikey")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if d.gotKey != "qkey" {
		t.Fatalf("decider got key=%q, want qkey (query wins over X-Original-URI)", d.gotKey)
	}
}

func TestCheck_IPValidation(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"1.2.3.4", "1.2.3.4"},
		{"  1.2.3.4  ", "1.2.3.4"},   // whitespace trimmed
		{"::1", "::1"},                // IPv6 loopback
		{"2001:db8::1", "2001:db8::1"},
		{"", ""},
		{"not-an-ip", ""},             // garbage rejected
		{"../etc/passwd", ""},         // path-traversal style rejected
		{"1.2.3.4, 5.6.7.8", ""},      // X-Forwarded-For style not accepted
		{"999.999.999.999", ""},       // out-of-range IPv4 rejected
	}
	for _, tc := range cases {
		d := &spyDecider{allow: true}
		h := newTestCheck(d)
		r := httptest.NewRequest("GET", "/check", nil)
		r.Header.Set("X-Api-Key", "k") // non-empty so handler uses extractIP path too
		r.Header.Set("X-Real-IP", tc.header)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if d.gotIP != tc.want {
			t.Errorf("X-Real-IP=%q: got ip=%q want %q", tc.header, d.gotIP, tc.want)
		}
	}
}

func TestCheck_NoHeadersFailOpenWithoutKey(t *testing.T) {
	d := &spyDecider{allow: true}
	h := newTestCheck(d)
	r := httptest.NewRequest("GET", "/check", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if d.gotKey != "" || d.gotIP != "" {
		t.Fatalf("expected empty key/ip, got key=%q ip=%q", d.gotKey, d.gotIP)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (decider returned allow), got %d", w.Code)
	}
}
