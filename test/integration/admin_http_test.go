// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Covers §17.2 F — the admin UI at the HTTP level: status codes,
// rendered anchors, search, pagination, sorting, CSRF and the two-step
// purge.

// TestAdminHTTP_PagesRender — all four pages answer 200 and carry their
// own heading.
func TestAdminHTTP_PagesRender(t *testing.T) {
	s := spawnService(t)

	pages := map[string]string{
		"/":           "api-ratelimiter — status",
		"/limits":     "redisDB1 — limits",
		"/abuse/keys": "redisDB2 — abusive api keys",
		"/abuse/ips":  "redisDB3 — abusive IPs",
	}
	for path, anchor := range pages {
		code, body := s.adminBody(path)
		if code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, code)
			continue
		}
		if !strings.Contains(body, anchor) {
			t.Errorf("GET %s: body missing anchor %q", path, anchor)
		}
		if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
			t.Errorf("GET %s: body does not end with </html> — render aborted mid-template", path)
		}
	}
}

// TestAdminHTTP_StatusPageWithRedisDown — the status page must still
// render (200) and report the outage instead of erroring out.
func TestAdminHTTP_StatusPageWithRedisDown(t *testing.T) {
	s := spawnService(t, withoutRedis())

	code, body := s.adminBody("/")
	if code != http.StatusOK {
		t.Fatalf("GET / with Redis down: status = %d, want 200", code)
	}
	if !strings.Contains(body, "unreachable") {
		t.Errorf("GET / with Redis down: body does not report the outage")
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
		t.Errorf("GET /: body does not end with </html>")
	}
}

// TestAdminHTTP_TablesShowRedisEntries — data seeded straight into
// redisDB1/2/3 shows up in the corresponding table.
func TestAdminHTTP_TablesShowRedisEntries(t *testing.T) {
	s := spawnService(t)
	s.seedLimit("seeded-limit", 42)
	s.seedAbuseKey("seeded-abuse-key")
	s.seedAbuseIP("203.0.113.42")

	if keys := s.storedKeys("/limits"); !containsStr(keys, "seeded-limit") {
		t.Errorf("/limits rows = %v, want to contain seeded-limit", keys)
	}
	if _, body := s.adminBody("/limits"); !strings.Contains(body, "42") {
		t.Errorf("/limits does not render the limit value")
	}
	if keys := s.storedKeys("/abuse/keys"); !containsStr(keys, "seeded-abuse-key") {
		t.Errorf("/abuse/keys rows = %v, want to contain seeded-abuse-key", keys)
	}
	if keys := s.storedKeys("/abuse/ips"); !containsStr(keys, "203.0.113.42") {
		t.Errorf("/abuse/ips rows = %v, want to contain 203.0.113.42", keys)
	}
}

// TestAdminHTTP_SearchFilters — ?q= is a case-sensitive substring
// filter over the Redis-backed table.
func TestAdminHTTP_SearchFilters(t *testing.T) {
	s := spawnService(t)
	for _, k := range []string{"alpha-1", "alpha-2", "beta-1"} {
		s.seedLimit(k, 10)
	}

	all := s.storedKeys("/limits")
	if len(all) != 3 {
		t.Fatalf("/limits rows = %v, want 3", all)
	}
	if got := s.storedKeys("/limits?q=alpha"); len(got) != 2 {
		t.Errorf("/limits?q=alpha rows = %v, want the two alpha keys", got)
	}
	if got := s.storedKeys("/limits?q=ALPHA"); len(got) != 0 {
		t.Errorf("/limits?q=ALPHA rows = %v, want none (search is case-sensitive)", got)
	}
	if got := s.storedKeys("/limits?q=beta-1"); len(got) != 1 || got[0] != "beta-1" {
		t.Errorf("/limits?q=beta-1 rows = %v, want [beta-1]", got)
	}
}

// TestAdminHTTP_Pagination — 26 entries split into a full first page
// and a one-row second page (pageSize is 25).
func TestAdminHTTP_Pagination(t *testing.T) {
	const total = 26
	s := spawnService(t)
	for i := 0; i < total; i++ {
		s.seedLimit(fmt.Sprintf("paged-%02d", i), 10)
	}

	if got := s.storedKeys("/limits"); len(got) != 25 {
		t.Errorf("page 1 rows = %d, want 25", len(got))
	}
	page2 := s.storedKeys("/limits?page=2")
	if len(page2) != 1 {
		t.Fatalf("page 2 rows = %d, want 1", len(page2))
	}
	if page2[0] != "paged-25" {
		t.Errorf("page 2 row = %q, want paged-25 (rows are sorted by key)", page2[0])
	}
	// Out-of-range pages render empty rather than erroring.
	if code, _ := s.adminBody("/limits?page=99"); code != http.StatusOK {
		t.Errorf("GET /limits?page=99: status = %d, want 200", code)
	}
}

// TestAdminHTTP_TopSortOrder — ?topsort= reorders the in-memory top
// table. Two KnownCounters are built so that "most total requests" and
// "most violations" are different keys.
func TestAdminHTTP_TopSortOrder(t *testing.T) {
	s := spawnService(t,
		withFlag("--window", "second"),
		withFlag("--burst", "0"),
		withFlag("--global-limit", "100"),
	)
	s.seedLimit("many-requests", 100) // never over its limit
	s.seedLimit("many-violations", 1) // over its limit in every slot

	// 10 requests in one slot: high total, zero violations.
	s.checkN(10, map[string]string{"X-Api-Key": "many-requests"})
	// Two slots of 2 requests: low total, but a violation is recorded
	// when the slot changes (WindowCount 2 > limit 1).
	for i := 0; i < 3; i++ {
		sleepToNextSecond(20 * time.Millisecond)
		s.checkN(2, map[string]string{"X-Api-Key": "many-violations"})
	}

	byTotal := s.topKeys("/limits?topsort=total")
	if len(byTotal) < 2 || byTotal[0] != "many-requests" {
		t.Errorf("?topsort=total order = %v, want many-requests first", byTotal)
	}
	byViolations := s.topKeys("/limits?topsort=violations")
	if len(byViolations) < 2 || byViolations[0] != "many-violations" {
		t.Errorf("?topsort=violations order = %v, want many-violations first", byViolations)
	}
	// Unknown sort values fall back to the default (violations).
	if got := s.topKeys("/limits?topsort=nonsense"); len(got) < 2 || got[0] != "many-violations" {
		t.Errorf("?topsort=nonsense order = %v, want the default violations order", got)
	}
}

// TestAdminHTTP_CSRFRequired — state-changing POSTs need the
// process-scoped token (invariant 8).
func TestAdminHTTP_CSRFRequired(t *testing.T) {
	s := spawnService(t)
	s.seedLimit("csrf-victim", 1)

	endpoints := []string{
		"/limits/delete", "/limits/purge",
		"/abuse/keys/delete", "/abuse/keys/purge",
		"/abuse/ips/delete", "/abuse/ips/purge",
	}
	for _, ep := range endpoints {
		t.Run("no token "+ep, func(t *testing.T) {
			if code, _ := s.adminPOST(ep, url.Values{"keys": {"csrf-victim"}}); code != http.StatusForbidden {
				t.Errorf("POST %s without token: status = %d, want 403", ep, code)
			}
		})
		t.Run("bad token "+ep, func(t *testing.T) {
			form := url.Values{"keys": {"csrf-victim"}, "csrf_token": {strings.Repeat("0", 64)}}
			if code, _ := s.adminPOST(ep, form); code != http.StatusForbidden {
				t.Errorf("POST %s with a bogus token: status = %d, want 403", ep, code)
			}
		})
	}

	// Nothing was deleted while the tokens were wrong.
	if keys := s.keys(1, "rate:limit:*"); len(keys) != 1 {
		t.Errorf("redisDB1 = %v, want the entry to survive rejected CSRF posts", keys)
	}
}

// TestAdminHTTP_DeleteWithValidToken — the happy path for each of the
// three delete endpoints: 303 back to the list page, entry gone from
// Redis.
func TestAdminHTTP_DeleteWithValidToken(t *testing.T) {
	s := spawnService(t)
	s.seedLimit("del-limit", 5)
	s.seedLimit("keep-limit", 5)
	s.seedAbuseKey("del-abuse-key")
	s.seedAbuseIP("203.0.113.7")

	cases := []struct {
		page, action, key, redisKey string
		db                          int
	}{
		{"/limits", "/limits/delete", "del-limit", "rate:limit:del-limit", 1},
		{"/abuse/keys", "/abuse/keys/delete", "del-abuse-key", "rate:abuse:key:del-abuse-key", 2},
		{"/abuse/ips", "/abuse/ips/delete", "203.0.113.7", "rate:abuse:ip:203.0.113.7", 3},
	}
	for _, c := range cases {
		t.Run(c.action, func(t *testing.T) {
			token := s.csrfToken(c.page)
			code, body := s.adminPOST(c.action, url.Values{
				"keys":       {c.key},
				"csrf_token": {token},
			})
			if code != http.StatusSeeOther {
				t.Fatalf("POST %s: status = %d, want 303\n%s", c.action, code, body)
			}
			if _, ok := s.hgetAll(c.db, c.redisKey); ok {
				t.Errorf("%s still present in redisDB%d after delete", c.redisKey, c.db)
			}
		})
	}

	if _, ok := s.hgetAll(1, "rate:limit:keep-limit"); !ok {
		t.Errorf("unrelated entry was deleted too")
	}
}

// TestAdminHTTP_DeleteEmptySelection — a valid POST with no checkboxes
// selected redirects without touching Redis.
func TestAdminHTTP_DeleteEmptySelection(t *testing.T) {
	s := spawnService(t)
	s.seedLimit("untouched", 5)

	token := s.csrfToken("/limits")
	code, _ := s.adminPOST("/limits/delete", url.Values{"csrf_token": {token}})
	if code != http.StatusSeeOther {
		t.Errorf("POST /limits/delete with no selection: status = %d, want 303", code)
	}
	if _, ok := s.hgetAll(1, "rate:limit:untouched"); !ok {
		t.Errorf("entry disappeared on an empty-selection delete")
	}
}

// TestAdminHTTP_TwoStepPurge — the first POST renders the confirmation
// page (carrying the token through), the second one with confirm=yes
// flushes the database.
func TestAdminHTTP_TwoStepPurge(t *testing.T) {
	s := spawnService(t)
	s.seedLimit("purge-1", 1)
	s.seedLimit("purge-2", 2)
	s.seedAbuseKey("purge-abuse-key")
	s.seedAbuseIP("203.0.113.8")

	cases := []struct {
		page, action, pattern string
		db                    int
	}{
		{"/limits", "/limits/purge", "rate:limit:*", 1},
		{"/abuse/keys", "/abuse/keys/purge", "rate:abuse:key:*", 2},
		{"/abuse/ips", "/abuse/ips/purge", "rate:abuse:ip:*", 3},
	}
	for _, c := range cases {
		t.Run(c.action, func(t *testing.T) {
			token := s.csrfToken(c.page)

			code, body := s.adminPOST(c.action, url.Values{"csrf_token": {token}})
			if code != http.StatusOK {
				t.Fatalf("first POST %s: status = %d, want 200 (confirmation page)", c.action, code)
			}
			if !strings.Contains(body, "Permanently delete") {
				t.Errorf("first POST %s: no confirmation page rendered\n%s", c.action, body)
			}
			if keys := s.keys(c.db, c.pattern); len(keys) == 0 {
				t.Fatalf("first POST %s already flushed redisDB%d", c.action, c.db)
			}

			code, body = s.adminPOST(c.action, url.Values{
				"csrf_token": {token},
				"confirm":    {"yes"},
			})
			if code != http.StatusSeeOther {
				t.Fatalf("confirmed POST %s: status = %d, want 303\n%s", c.action, code, body)
			}
			if keys := s.keys(c.db, c.pattern); len(keys) != 0 {
				t.Errorf("redisDB%d = %v after purge, want empty", c.db, keys)
			}
		})
	}
}

// TestAdminHTTP_MethodAndPathErrors — GET on an action endpoint is 405,
// an unknown path is 404.
func TestAdminHTTP_MethodAndPathErrors(t *testing.T) {
	s := spawnService(t)

	for _, ep := range []string{
		"/limits/delete", "/limits/purge",
		"/abuse/keys/delete", "/abuse/keys/purge",
		"/abuse/ips/delete", "/abuse/ips/purge",
	} {
		if code, _ := s.adminBody(ep); code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: status = %d, want 405", ep, code)
		}
	}

	for _, p := range []string{"/nope", "/limits/nope", "/abuse", "/abuse/keys/nope"} {
		if code, _ := s.adminBody(p); code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", p, code)
		}
	}
}
