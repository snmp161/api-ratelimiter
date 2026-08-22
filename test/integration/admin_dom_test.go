// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

// Covers §17.2 G — the admin UI at the DOM level. goquery parses the
// real rendered HTML, so these tests fail on broken markup that a
// substring match would happily accept. No headless browser: the only
// JS in the UI is the optional actions widget, which has a full
// server-side fallback (see §17.2 preamble).

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TestAdminDOM_StatusPageStructure — heading and navigation.
func TestAdminDOM_StatusPageStructure(t *testing.T) {
	s := spawnService(t)
	doc := s.adminDoc("/")

	if got := strings.TrimSpace(doc.Find("h1").First().Text()); got != "api-ratelimiter — status" {
		t.Errorf("h1 = %q, want %q", got, "api-ratelimiter — status")
	}

	want := []string{"/", "/limits", "/abuse/keys", "/abuse/ips"}
	var hrefs []string
	doc.Find("nav a").Each(func(_ int, a *goquery.Selection) {
		if href, ok := a.Attr("href"); ok {
			hrefs = append(hrefs, href)
		}
	})
	if len(hrefs) != len(want) {
		t.Fatalf("nav links = %v, want %v", hrefs, want)
	}
	for i, w := range want {
		if hrefs[i] != w {
			t.Errorf("nav link %d = %q, want %q", i, hrefs[i], w)
		}
	}

	// The flags table renders the effective configuration.
	if !strings.Contains(doc.Text(), "--global-limit") {
		t.Errorf("status page does not list the flags")
	}
}

// TestAdminDOM_ActionFormWiring — every list page carries an action
// form with a 64-hex CSRF token and Delete/Purge buttons pointed at the
// right endpoints via formaction.
func TestAdminDOM_ActionFormWiring(t *testing.T) {
	s := spawnService(t)

	cases := []struct{ page, del, purge string }{
		{"/limits", "/limits/delete", "/limits/purge"},
		{"/abuse/keys", "/abuse/keys/delete", "/abuse/keys/purge"},
		{"/abuse/ips", "/abuse/ips/delete", "/abuse/ips/purge"},
	}
	for _, c := range cases {
		t.Run(c.page, func(t *testing.T) {
			doc := s.adminDoc(c.page)

			form := doc.Find("form#action-form")
			if form.Length() != 1 {
				t.Fatalf("form#action-form count = %d, want 1", form.Length())
			}
			if method, _ := form.Attr("method"); !strings.EqualFold(method, "post") {
				t.Errorf("action form method = %q, want post", method)
			}

			token, ok := form.Find("input[name=csrf_token]").First().Attr("value")
			if !ok {
				t.Fatalf("no hidden csrf_token input in the action form")
			}
			if !hex64.MatchString(token) {
				t.Errorf("csrf_token = %q, want 64 hex chars", token)
			}

			var actions []string
			doc.Find("button[formaction]").Each(func(_ int, b *goquery.Selection) {
				a, _ := b.Attr("formaction")
				actions = append(actions, a)
			})
			for _, want := range []string{c.del, c.purge} {
				if !containsStr(actions, want) {
					t.Errorf("no button with formaction %q (got %v)", want, actions)
				}
			}
		})
	}
}

// TestAdminDOM_LimitRowRendered — a redisDB1 entry becomes a row whose
// first <code> cell is the api_key and which carries the limit value
// and a selection checkbox.
func TestAdminDOM_LimitRowRendered(t *testing.T) {
	s := spawnService(t)
	s.seedLimit("dom-key", 77)

	doc := s.adminDoc("/limits")
	var row *goquery.Selection
	doc.Find("form#action-form tr").EachWithBreak(func(_ int, tr *goquery.Selection) bool {
		if strings.TrimSpace(tr.Find("code").First().Text()) == "dom-key" {
			row = tr
			return false
		}
		return true
	})
	if row == nil {
		t.Fatalf("no <tr> for dom-key on /limits")
	}
	if !strings.Contains(row.Text(), "77") {
		t.Errorf("row text = %q, want it to contain the limit 77", row.Text())
	}
	cb := row.Find("input[type=checkbox][name=keys]")
	if v, _ := cb.Attr("value"); v != "dom-key" {
		t.Errorf("checkbox value = %q, want dom-key", v)
	}
}

// TestAdminDOM_SearchAndPager — ?q= narrows the DOM rows and the pager
// links reflect where the operator is.
func TestAdminDOM_SearchAndPager(t *testing.T) {
	s := spawnService(t)
	for i := 0; i < 26; i++ {
		s.seedLimit(fmt.Sprintf("pager-%02d", i), 10)
	}
	s.seedLimit("odd-one-out", 10)

	if rows := s.storedKeys("/limits?q=odd"); len(rows) != 1 || rows[0] != "odd-one-out" {
		t.Errorf("?q=odd rows = %v, want [odd-one-out]", rows)
	}

	// Page 1: prev disabled, next is a link to page 2.
	doc := s.adminDoc("/limits")
	if got := doc.Find(".pager span.disabled").First().Text(); !strings.Contains(got, "prev") {
		t.Errorf("page 1: prev = %q, want a disabled ← prev", got)
	}
	next, ok := doc.Find(".pager a").First().Attr("href")
	if !ok || !strings.Contains(next, "page=2") {
		t.Errorf("page 1: next href = %q (present=%v), want ?page=2", next, ok)
	}

	// Page 2: prev is a link back, next is disabled.
	doc = s.adminDoc("/limits?page=2")
	prev, ok := doc.Find(".pager a").First().Attr("href")
	if !ok || !strings.Contains(prev, "page=1") {
		t.Errorf("page 2: prev href = %q (present=%v), want ?page=1", prev, ok)
	}
	if got := doc.Find(".pager span.disabled").First().Text(); !strings.Contains(got, "next") {
		t.Errorf("page 2: next = %q, want a disabled next →", got)
	}
}

// TestAdminDOM_TopSortIndicator — the active top-25 sort column is
// marked with ↓ and is not a link; the others are links that switch.
func TestAdminDOM_TopSortIndicator(t *testing.T) {
	s := spawnService(t)
	s.seedLimit("sorted-key", 10)
	s.mustCheck(map[string]string{"X-Api-Key": "sorted-key"})

	headerFor := func(path string) []string {
		doc := s.adminDoc(path)
		var out []string
		doc.Find("h2").Each(func(_ int, h *goquery.Selection) {
			if !strings.Contains(h.Text(), "in-memory") {
				return
			}
			h.NextAllFiltered("table").First().Find("th").Each(func(_ int, th *goquery.Selection) {
				out = append(out, strings.TrimSpace(th.Text()))
			})
		})
		return out
	}

	for _, tc := range []struct{ sort, want string }{
		{"total", "total ↓"},
		{"burst", "burst ↓"},
		{"violations", "violations ↓"},
	} {
		headers := headerFor("/limits?topsort=" + tc.sort)
		if !containsStr(headers, tc.want) {
			t.Errorf("?topsort=%s: headers = %v, want one to be %q", tc.sort, headers, tc.want)
		}
		// Exactly one column may claim the sort.
		marked := 0
		for _, h := range headers {
			if strings.Contains(h, "↓") {
				marked++
			}
		}
		if marked != 1 {
			t.Errorf("?topsort=%s: %d columns marked with ↓, want exactly 1 (%v)", tc.sort, marked, headers)
		}
	}
}

// TestAdminDOM_CSRFFlowEndToEnd — read the token out of the rendered
// form like a browser would, submit it, and verify the effect in Redis.
func TestAdminDOM_CSRFFlowEndToEnd(t *testing.T) {
	s := spawnService(t)
	s.seedLimit("dom-delete-me", 3)
	s.seedLimit("dom-keep-me", 3)

	doc := s.adminDoc("/limits")
	token, ok := doc.Find("input[name=csrf_token]").First().Attr("value")
	if !ok {
		t.Fatalf("no csrf_token in the DOM")
	}
	// Submit exactly what the browser would: the token plus the value
	// of the checked checkbox.
	var value string
	doc.Find("input[type=checkbox][name=keys]").EachWithBreak(func(_ int, cb *goquery.Selection) bool {
		if v, _ := cb.Attr("value"); v == "dom-delete-me" {
			value = v
			return false
		}
		return true
	})
	if value == "" {
		t.Fatalf("no checkbox for dom-delete-me")
	}

	code, body := s.adminPOST("/limits/delete", url.Values{
		"csrf_token": {token},
		"keys":       {value},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("delete: status = %d, want 303\n%s", code, body)
	}
	if _, ok := s.hgetAll(1, "rate:limit:dom-delete-me"); ok {
		t.Errorf("rate:limit:dom-delete-me survived the delete")
	}
	if _, ok := s.hgetAll(1, "rate:limit:dom-keep-me"); !ok {
		t.Errorf("rate:limit:dom-keep-me was deleted too")
	}
}

// TestAdminDOM_PurgeConfirmPage — the confirmation page is a real form
// carrying the token and confirm=yes, pointed back at the purge
// endpoint.
func TestAdminDOM_PurgeConfirmPage(t *testing.T) {
	s := spawnService(t)
	s.seedLimit("to-purge", 1)

	token := s.csrfToken("/limits")
	code, body := s.adminPOST("/limits/purge", url.Values{"csrf_token": {token}})
	if code != http.StatusOK {
		t.Fatalf("purge step 1: status = %d, want 200", code)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse confirm page: %v", err)
	}
	if !strings.Contains(doc.Text(), "Permanently delete") {
		t.Errorf("confirm page does not warn about the consequences")
	}
	form := doc.Find("form[action='/limits/purge']")
	if form.Length() != 1 {
		t.Fatalf("confirm form count = %d, want 1", form.Length())
	}
	if v, _ := form.Find("input[name=confirm]").Attr("value"); v != "yes" {
		t.Errorf("hidden confirm = %q, want yes", v)
	}
	if v, _ := form.Find("input[name=csrf_token]").Attr("value"); v != token {
		t.Errorf("confirm page carries a different csrf token")
	}
	// The entry is still there — nothing has been flushed yet.
	if _, ok := s.hgetAll(1, "rate:limit:to-purge"); !ok {
		t.Errorf("purge happened before confirmation")
	}
}

// TestAdminDOM_HTMLEscaping — an api_key containing markup is rendered
// as text, not as HTML. html/template does this for us; the test makes
// sure nobody swaps in text/template or marks a field as
// template.HTML.
func TestAdminDOM_HTMLEscaping(t *testing.T) {
	const evil = `<script>alert(1)</script>`
	s := spawnService(t)
	s.seedLimit(evil, 1)
	s.seedAbuseKey(evil)

	for _, page := range []string{"/limits", "/abuse/keys"} {
		code, body := s.adminBody(page)
		if code != http.StatusOK {
			t.Fatalf("GET %s: status = %d", page, code)
		}
		if !strings.Contains(body, "&lt;script&gt;") {
			t.Errorf("%s: the key is not HTML-escaped in the output", page)
		}
		if strings.Contains(body, "<script>alert(1)</script>") {
			t.Errorf("%s: raw <script> from the api_key made it into the page", page)
		}

		doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
		if err != nil {
			t.Fatalf("parse %s: %v", page, err)
		}
		// The only legitimate <script> is the actions widget.
		scripts := doc.Find("script")
		if scripts.Length() != 1 {
			t.Errorf("%s: <script> count = %d, want 1 (actions_script only)", page, scripts.Length())
		}
		if strings.Contains(scripts.Text(), "alert(1)") {
			t.Errorf("%s: injected code ended up inside a <script>", page)
		}
		// …and the key still renders as text in a row.
		if !containsStr(s.storedKeys(page), evil) {
			t.Errorf("%s: escaped key is missing from the table", page)
		}
	}
}
