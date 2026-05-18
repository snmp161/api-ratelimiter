// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	dto "github.com/prometheus/client_model/go"

	"ratelimiter/internal/config"
	"ratelimiter/internal/counter"
	"ratelimiter/internal/metrics"
	"ratelimiter/internal/store"
)

func newTestServer(t *testing.T) (*Server, *miniredis.Miniredis, *counter.KnownMap, *counter.UnknownMap) {
	t.Helper()
	mr := miniredis.RunT(t)
	s := store.New(mr.Addr(), "", nil)
	t.Cleanup(s.Close)
	cfg := config.Default()
	known := counter.NewKnownMap(1, nil)
	unknown := counter.NewUnknownMap(1, nil)
	m := metrics.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, known, unknown, s, m, logger, time.Unix(1700000000, 0), "test")
	if err != nil {
		t.Fatalf("server init: %v", err)
	}
	return srv, mr, known, unknown
}

// extractCSRF pulls the token out of a rendered page so subsequent POSTs
// can hit the validated path end-to-end (not just by reading s.csrfToken).
var csrfRe = regexp.MustCompile(`name="csrf_token" value="([0-9a-f]+)"`)

func extractCSRF(t *testing.T, body string) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(body)
	if len(m) != 2 {
		t.Fatalf("csrf token not found in body")
	}
	return m[1]
}

func TestIndex_RendersOK_WithRedisUp(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "test") { // version embedded
		t.Errorf("version not rendered")
	}
	if !strings.Contains(w.Body.String(), "ok") && !strings.Contains(w.Body.String(), "status") {
		t.Errorf("body doesn't look like status page: %q", w.Body.String()[:200])
	}
}

func TestIndex_RendersWhenRedisDown(t *testing.T) {
	srv, mr, _, _ := newTestServer(t)
	mr.Close() // kill Redis before the request
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (page must render even with redis down)", w.Code)
	}
}

func TestLimits_RendersWithEntries(t *testing.T) {
	srv, mr, _, _ := newTestServer(t)
	mr.Select(store.DBLimits)
	mr.HSet("rate:limit:abc", "limit", "500", "created_at", "1700000000")

	r := httptest.NewRequest("GET", "/limits", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "abc") {
		t.Errorf("api key not in body")
	}
	if !strings.Contains(body, "500") {
		t.Errorf("limit not in body")
	}
	if !strings.Contains(body, `name="csrf_token"`) {
		t.Errorf("csrf hidden input missing")
	}
}

func TestDelete_RequiresPOST(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	r := httptest.NewRequest("GET", "/limits/delete", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405", w.Code)
	}
}

func TestDelete_RejectsBadCSRF(t *testing.T) {
	srv, mr, _, _ := newTestServer(t)
	mr.Select(store.DBLimits)
	mr.HSet("rate:limit:k", "limit", "1")

	form := url.Values{"keys": {"k"}, "csrf_token": {"deadbeef"}}
	r := httptest.NewRequest("POST", "/limits/delete", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", w.Code)
	}
	if !mr.Exists("rate:limit:k") {
		t.Fatal("key must remain after CSRF rejection")
	}
}

func TestDelete_WithValidCSRF_RemovesKeys(t *testing.T) {
	srv, mr, _, _ := newTestServer(t)
	mr.Select(store.DBLimits)
	mr.HSet("rate:limit:a", "limit", "1")
	mr.HSet("rate:limit:b", "limit", "2")
	mr.HSet("rate:limit:c", "limit", "3")

	// Get a CSRF token from a real page render.
	r := httptest.NewRequest("GET", "/limits", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	token := extractCSRF(t, w.Body.String())

	form := url.Values{"keys": {"a", "c"}, "csrf_token": {token}}
	r = httptest.NewRequest("POST", "/limits/delete", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d want 303", w.Code)
	}
	if mr.Exists("rate:limit:a") || mr.Exists("rate:limit:c") {
		t.Fatal("a and c must be deleted")
	}
	if !mr.Exists("rate:limit:b") {
		t.Fatal("b must remain")
	}
}

func TestDelete_EmptyKeysRedirectsBack(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	form := url.Values{"csrf_token": {srv.csrfToken}}
	r := httptest.NewRequest("POST", "/limits/delete", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d want 303 (empty selection is a no-op redirect)", w.Code)
	}
}

func TestPurge_FirstPOSTRendersConfirm(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	form := url.Values{"csrf_token": {srv.csrfToken}}
	r := httptest.NewRequest("POST", "/limits/purge", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (confirm page)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Permanently delete") {
		t.Errorf("confirm page body unexpected: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `name="csrf_token"`) {
		t.Errorf("confirm page must carry csrf token forward")
	}
}

func TestPurge_ConfirmYesFlushesDB(t *testing.T) {
	srv, mr, _, _ := newTestServer(t)
	mr.Select(store.DBLimits)
	mr.HSet("rate:limit:a", "limit", "1")
	mr.HSet("rate:limit:b", "limit", "2")

	form := url.Values{"csrf_token": {srv.csrfToken}, "confirm": {"yes"}}
	r := httptest.NewRequest("POST", "/limits/purge", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d want 303", w.Code)
	}
	if len(mr.DB(store.DBLimits).Keys()) != 0 {
		t.Fatal("DBLimits must be empty after purge")
	}
}

func TestPurge_RejectsBadCSRF(t *testing.T) {
	srv, mr, _, _ := newTestServer(t)
	mr.Select(store.DBLimits)
	mr.HSet("rate:limit:a", "limit", "1")

	form := url.Values{"csrf_token": {"wrong"}, "confirm": {"yes"}}
	r := httptest.NewRequest("POST", "/limits/purge", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", w.Code)
	}
	if !mr.Exists("rate:limit:a") {
		t.Fatal("key must remain after CSRF rejection")
	}
}

func TestAbuseKeys_AndIPs_RenderOK(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	for _, path := range []string{"/abuse/keys", "/abuse/ips"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("%s: status=%d want 200", path, w.Code)
		}
		if !strings.Contains(w.Body.String(), `name="csrf_token"`) {
			t.Errorf("%s: csrf hidden input missing", path)
		}
	}
}

func TestAbuseIPs_DeleteWithCSRF(t *testing.T) {
	srv, mr, _, _ := newTestServer(t)
	rec := store.AbuseRecord{TotalRequests: 5}
	if err := srv.store.UpsertAbuseIP(context.Background(), "1.1.1.1", rec, time.Minute); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"csrf_token": {srv.csrfToken}, "keys": {"1.1.1.1"}}
	r := httptest.NewRequest("POST", "/abuse/ips/delete", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d want 303", w.Code)
	}
	mr.Select(store.DBAbuseIP)
	if mr.Exists("rate:abuse:ip:1.1.1.1") {
		t.Fatal("1.1.1.1 must be deleted")
	}
}

func TestAbuseIPs_PurgeConfirmFlow(t *testing.T) {
	srv, mr, _, _ := newTestServer(t)
	if err := srv.store.UpsertAbuseIP(context.Background(), "1.1.1.1", store.AbuseRecord{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	// First POST without confirm → confirm page.
	form := url.Values{"csrf_token": {srv.csrfToken}}
	r := httptest.NewRequest("POST", "/abuse/ips/purge", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "redisDB3") {
		t.Fatalf("confirm page expected for /abuse/ips/purge")
	}
	// Second POST with confirm=yes → FLUSHDB.
	form = url.Values{"csrf_token": {srv.csrfToken}, "confirm": {"yes"}}
	r = httptest.NewRequest("POST", "/abuse/ips/purge", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d want 303", w.Code)
	}
	if len(mr.DB(store.DBAbuseIP).Keys()) != 0 {
		t.Fatal("DBAbuseIP must be empty after purge")
	}
}

func TestAbuseKeys_PurgeConfirmFlow(t *testing.T) {
	srv, mr, _, _ := newTestServer(t)
	if err := srv.store.UpsertAbuseKey(context.Background(), "k1", store.AbuseRecord{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"csrf_token": {srv.csrfToken}, "confirm": {"yes"}}
	r := httptest.NewRequest("POST", "/abuse/keys/purge", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d want 303", w.Code)
	}
	if len(mr.DB(store.DBAbuseKey).Keys()) != 0 {
		t.Fatal("DBAbuseKey must be empty after purge")
	}
}

func TestTopSort_AllBranchesCovered(t *testing.T) {
	srv, mr, known, unknown := newTestServer(t)
	// Populate something on each page so the top-25 templates have rows.
	mr.Select(store.DBLimits)
	mr.HSet("rate:limit:a", "limit", "10", "created_at", "0")
	known.RecordRequest("a", 10, 0)
	unknown.RecordRequest("key:b", 10, 0, 10)
	unknown.RecordRequest("ip:1.2.3.4", 10, 0, 10)

	// Three pages × four topsort values (including "invalid" → default).
	pages := []string{"/limits", "/abuse/keys", "/abuse/ips"}
	sorts := []string{"total", "burst", "violations", "abuse", "garbage"}
	for _, p := range pages {
		for _, s := range sorts {
			code, body := renderGet(t, srv, p+"?topsort="+s)
			if code != http.StatusOK {
				t.Errorf("%s?topsort=%s: status=%d", p, s, code)
			}
			if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
				t.Errorf("%s?topsort=%s: body truncated", p, s)
			}
		}
	}
}

func TestAbuseKeys_DeleteWithCSRF(t *testing.T) {
	srv, mr, _, _ := newTestServer(t)
	rec := store.AbuseRecord{TotalRequests: 10}
	if err := mr.Set("noop", "noop"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.UpsertAbuseKey(httptest.NewRequest("", "/", nil).Context(), "k1", rec, time.Minute); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"csrf_token": {srv.csrfToken}, "keys": {"k1"}}
	r := httptest.NewRequest("POST", "/abuse/keys/delete", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status=%d want 303", w.Code)
	}
	mr.Select(store.DBAbuseKey)
	if mr.Exists("rate:abuse:key:k1") {
		t.Fatal("k1 must be deleted")
	}
}

func TestKnownTopRows_PopulatedFromInMemory(t *testing.T) {
	srv, mr, known, _ := newTestServer(t)
	mr.Select(store.DBLimits)
	mr.HSet("rate:limit:hot", "limit", "10")

	// Populate the in-memory KnownCounter for "hot".
	for i := 0; i < 20; i++ {
		known.RecordRequest("hot", 10, 0)
	}

	r := httptest.NewRequest("GET", "/limits", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, "hot") {
		t.Errorf("api key 'hot' not in body")
	}
	if !strings.Contains(body, "20") { // total requests
		t.Errorf("counter value not surfaced in body")
	}
}

func TestUnknownTopRows_FilteredByPrefix(t *testing.T) {
	srv, _, _, unknown := newTestServer(t)
	for i := 0; i < 5; i++ {
		unknown.RecordRequest("key:foo", 1, 0, 10)
		unknown.RecordRequest("ip:1.2.3.4", 1, 0, 10)
	}

	r := httptest.NewRequest("GET", "/abuse/keys", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, "foo") {
		t.Errorf("/abuse/keys must include unknown key 'foo'")
	}
	if strings.Contains(body, "1.2.3.4") {
		t.Errorf("/abuse/keys must NOT include ip:1.2.3.4 (wrong prefix)")
	}

	r = httptest.NewRequest("GET", "/abuse/ips", nil)
	w = httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	body = w.Body.String()
	if !strings.Contains(body, "1.2.3.4") {
		t.Errorf("/abuse/ips must include 1.2.3.4")
	}
	if strings.Contains(body, "foo") {
		t.Errorf("/abuse/ips must NOT include key:foo")
	}
}

func TestTopKnown_DeterministicOrderOnTies(t *testing.T) {
	srv, _, known, _ := newTestServer(t)
	// All counters have ViolationHits=0 → primary sort ties for every
	// pair. Without the Key tiebreaker, map iteration randomness would
	// make the rendered order differ from one call to the next.
	for _, k := range []string{"key_d", "key_a", "key_c", "key_b"} {
		known.RecordRequest(k, 100, 0)
	}

	render := func() string {
		r := httptest.NewRequest("GET", "/limits", nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, r)
		return w.Body.String()
	}
	first := render()
	for i := 0; i < 5; i++ {
		if render() != first {
			t.Fatal("rendered output changes between calls — top-25 not deterministic")
		}
	}
	// Verify the order: alphabetical by Key when violations are all 0.
	keys := []string{"key_a", "key_b", "key_c", "key_d"}
	last := -1
	for _, k := range keys {
		idx := strings.Index(first, k)
		if idx < 0 {
			t.Fatalf("key %q missing from rendered top-25", k)
		}
		if idx <= last {
			t.Errorf("keys not in alphabetical order: %q at %d should follow earlier keys", k, idx)
		}
		last = idx
	}
}

func TestTopUnknown_DeterministicOrderOnTies(t *testing.T) {
	srv, _, _, unknown := newTestServer(t)
	for _, k := range []string{"ip:9.9.9.9", "ip:1.1.1.1", "ip:5.5.5.5"} {
		unknown.RecordRequest(k, 100, 0, 10)
	}
	render := func() string {
		r := httptest.NewRequest("GET", "/abuse/ips", nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, r)
		return w.Body.String()
	}
	first := render()
	for i := 0; i < 5; i++ {
		if render() != first {
			t.Fatal("top-25 on /abuse/ips not deterministic on ties")
		}
	}
}

func TestNotFound(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	r := httptest.NewRequest("GET", "/nope", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

// ───── formatMetricValue per-type tests ─────────────────────────────

func TestFormatMetricValue_Counter(t *testing.T) {
	m := &dto.Metric{Counter: &dto.Counter{Value: float64Ptr(42)}}
	got := formatMetricValue(m, dto.MetricType_COUNTER)
	if got != "42" {
		t.Errorf("Counter: got %q want %q", got, "42")
	}
}

func TestFormatMetricValue_Gauge(t *testing.T) {
	m := &dto.Metric{Gauge: &dto.Gauge{Value: float64Ptr(3.14)}}
	got := formatMetricValue(m, dto.MetricType_GAUGE)
	if got != "3.14" {
		t.Errorf("Gauge: got %q want %q", got, "3.14")
	}
}

func TestFormatMetricValue_Histogram(t *testing.T) {
	m := &dto.Metric{Histogram: &dto.Histogram{
		SampleCount: uint64Ptr(100),
		SampleSum:   float64Ptr(12.5),
	}}
	got := formatMetricValue(m, dto.MetricType_HISTOGRAM)
	if !strings.Contains(got, "count=100") || !strings.Contains(got, "sum=12.500000s") {
		t.Errorf("Histogram: got %q", got)
	}
}

func TestFormatMetricValue_Summary(t *testing.T) {
	m := &dto.Metric{Summary: &dto.Summary{
		SampleCount: uint64Ptr(50),
		SampleSum:   float64Ptr(7.25),
	}}
	got := formatMetricValue(m, dto.MetricType_SUMMARY)
	if !strings.Contains(got, "count=50") || !strings.Contains(got, "sum=7.250000") {
		t.Errorf("Summary: got %q", got)
	}
}

func TestFormatMetricValue_UnsupportedType(t *testing.T) {
	got := formatMetricValue(&dto.Metric{}, dto.MetricType_UNTYPED)
	if !strings.HasPrefix(got, "unsupported:") {
		t.Errorf("UNTYPED: expected 'unsupported:' prefix, got %q", got)
	}
}

func TestFormatMetricValue_NilSubmessages(t *testing.T) {
	// Defensive: declared type says HISTOGRAM/SUMMARY but the
	// submessage is nil. Should return "" rather than panicking.
	if got := formatMetricValue(&dto.Metric{}, dto.MetricType_HISTOGRAM); got != "" {
		t.Errorf("nil Histogram: got %q want empty", got)
	}
	if got := formatMetricValue(&dto.Metric{}, dto.MetricType_SUMMARY); got != "" {
		t.Errorf("nil Summary: got %q want empty", got)
	}
}

func float64Ptr(v float64) *float64 { return &v }
func uint64Ptr(v uint64) *uint64    { return &v }

// ───── Template edge-case rendering ─────────────────────────────────────
//
// These tests poke the four admin templates with empty/extreme/hostile
// data so we catch broken templates at test time rather than letting
// production users see a partial HTML response. Each test asserts the
// status is 200 (or 4xx for empty-CSRF cases) and that the body contains
// some well-known anchor, so a template that bails halfway is observable.

func renderGet(t *testing.T, srv *Server, path string) (int, string) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func TestRender_EmptyDatabaseRendersAllPages(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	cases := []struct {
		path   string
		anchor string // must appear in the rendered body
	}{
		{"/", "api-ratelimiter"},
		{"/limits", "redisDB1"},
		{"/abuse/keys", "redisDB2"},
		{"/abuse/ips", "redisDB3"},
	}
	for _, c := range cases {
		code, body := renderGet(t, srv, c.path)
		if code != http.StatusOK {
			t.Errorf("%s: status=%d want 200", c.path, code)
		}
		if !strings.Contains(body, c.anchor) {
			t.Errorf("%s: anchor %q missing — template likely broke", c.path, c.anchor)
		}
		// Sanity: body must look like a complete HTML page, not a
		// truncated render.
		if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
			t.Errorf("%s: body doesn't end with </html> — render aborted midway?", c.path)
		}
	}
}

func TestRender_SpecialCharactersInKeys_EscapedNotInjected(t *testing.T) {
	srv, mr, known, unknown := newTestServer(t)

	hostile := `<script>alert("xss")</script>&"'`

	// Put it in redisDB1 (limits page) and in the in-memory counters
	// (top-25 path) and as a Redis abuse key.
	mr.Select(store.DBLimits)
	mr.HSet("rate:limit:"+hostile, "limit", "10", "created_at", "1700000000")
	known.RecordRequest(hostile, 10, 0)
	unknown.RecordRequest("key:"+hostile, 10, 0, 10)
	unknown.RecordRequest("ip:"+hostile, 10, 0, 10) // not a valid IP but exercises the prefix path
	if err := srv.store.UpsertAbuseKey(context.Background(), hostile, store.AbuseRecord{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.UpsertAbuseIP(context.Background(), hostile, store.AbuseRecord{}, time.Minute); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/limits", "/abuse/keys", "/abuse/ips"} {
		code, body := renderGet(t, srv, path)
		if code != http.StatusOK {
			t.Errorf("%s: status=%d want 200", path, code)
		}
		// Verbatim <script> must not be present — html/template should
		// have escaped it to &lt;script&gt;. If we ever swap to
		// text/template by accident this catches it.
		if strings.Contains(body, "<script>alert") {
			t.Errorf("%s: unescaped <script> tag in body — XSS vulnerability", path)
		}
		if !strings.Contains(body, "&lt;script&gt;") {
			t.Errorf("%s: expected escaped &lt;script&gt;, body didn't contain it", path)
		}
	}
}

func TestRender_BoundaryCounterValues(t *testing.T) {
	srv, mr, _, _ := newTestServer(t)
	// Write absurd values to a limit row to test integer formatting on
	// the limits page.
	mr.Select(store.DBLimits)
	mr.HSet("rate:limit:big", "limit", strconv.FormatInt(math.MaxInt64, 10), "created_at", "0")
	mr.HSet("rate:limit:zero", "limit", "0", "created_at", "0")

	code, body := renderGet(t, srv, "/limits")
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200", code)
	}
	if !strings.Contains(body, strconv.FormatInt(math.MaxInt64, 10)) {
		t.Errorf("MaxInt64 limit not rendered")
	}
	// created_at == 0 → zero time → handler shows "—"
	if !strings.Contains(body, "—") {
		t.Errorf("zero CreatedAt should render as em dash, body lacks it")
	}
}

func TestRender_ZeroAbuseEntryRendersWithDashes(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	// AbuseRecord with all zero fields — exercises fmtTime/fmtDuration
	// on zero-value time and zero TTL.
	if err := srv.store.UpsertAbuseKey(context.Background(), "z", store.AbuseRecord{}, 0); err != nil {
		t.Fatal(err)
	}
	code, body := renderGet(t, srv, "/abuse/keys")
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200", code)
	}
	if !strings.Contains(body, "z") {
		t.Errorf("zero entry not rendered")
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
		t.Errorf("body truncated — template bailed on zero values?")
	}
}

func TestRender_ConfirmPage(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	form := url.Values{"csrf_token": {srv.csrfToken}}
	r := httptest.NewRequest("POST", "/abuse/keys/purge", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm page status=%d want 200", w.Code)
	}
	body := w.Body.String()
	checks := []string{"Permanently delete", "redisDB2", `name="confirm"`, `name="csrf_token"`, "Cancel"}
	for _, c := range checks {
		if !strings.Contains(body, c) {
			t.Errorf("confirm.html missing %q", c)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
		t.Errorf("confirm.html truncated")
	}
}

func TestRender_IndexShowsAllFlagRowsAndMetrics(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	_, body := renderGet(t, srv, "/")

	// Every flag from flagRows() must appear on the page.
	expectedFlags := []string{
		"--listen", "--socket-mode", "--admin-listen", "--metrics-listen",
		"--redis-addr", "--redis-password", "--log-level", "--log-format",
		"--global-limit", "--burst", "--window", "--cleanup-interval",
		"--abuse-ttl", "--abuse-multiplier", "--abuse-transfer-threshold",
	}
	for _, f := range expectedFlags {
		if !strings.Contains(body, f) {
			t.Errorf("index page missing flag row %q", f)
		}
	}

	// Every Prometheus metric registered in metrics.New() must appear.
	expectedMetrics := []string{
		"ratelimit_requests_total",
		"ratelimit_counters_known_active",
		"ratelimit_counters_unknown_active",
		"ratelimit_memory_bytes",
		"ratelimit_cleanup_runs_total",
		"ratelimit_check_duration_seconds",
	}
	for _, m := range expectedMetrics {
		if !strings.Contains(body, m) {
			t.Errorf("index page missing metric %q", m)
		}
	}
}

func TestRender_PaginationBoundary(t *testing.T) {
	srv, mr, _, _ := newTestServer(t)
	// Seed exactly pageSize+1 entries so page 1 fills and page 2 has one.
	mr.Select(store.DBLimits)
	for i := 0; i < pageSize+1; i++ {
		mr.HSet("rate:limit:k"+strconv.Itoa(i), "limit", "10", "created_at", "0")
	}

	_, body1 := renderGet(t, srv, "/limits?page=1")
	if !strings.Contains(body1, "next →") {
		t.Errorf("page=1: expected an enabled 'next →' link")
	}
	_, body2 := renderGet(t, srv, "/limits?page=2")
	if !strings.Contains(body2, "← prev") {
		t.Errorf("page=2: expected an enabled '← prev' link")
	}
	// page=999 — past end — must render without crashing the slice indexing.
	code, body := renderGet(t, srv, "/limits?page=999")
	if code != http.StatusOK {
		t.Errorf("page=999: status=%d want 200 (out-of-range page must not crash)", code)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
		t.Errorf("page=999: body truncated")
	}
}

func TestRender_SearchWithNoMatchesStillRendersFully(t *testing.T) {
	srv, mr, _, _ := newTestServer(t)
	mr.Select(store.DBLimits)
	mr.HSet("rate:limit:abc", "limit", "10", "created_at", "0")

	code, body := renderGet(t, srv, "/limits?q=nonexistent")
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200", code)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
		t.Errorf("search-no-match: body truncated")
	}
	if !strings.Contains(body, "no entries") {
		t.Errorf("empty-result page should show 'no entries'")
	}
}
