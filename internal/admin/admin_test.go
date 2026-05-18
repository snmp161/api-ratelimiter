package admin

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

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
