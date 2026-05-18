// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ratelimiter/internal/metrics"
)

type Decider interface {
	Decide(ctx context.Context, apiKey, ip string) bool
}

type Check struct {
	limiter Decider
	metrics *metrics.Metrics
	logger  *slog.Logger
}

func NewCheck(l Decider, m *metrics.Metrics, logger *slog.Logger) *Check {
	return &Check{limiter: l, metrics: m, logger: logger}
}

func (c *Check) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		c.metrics.CheckDurationSeconds.Observe(time.Since(start).Seconds())
		// Fail open: any panic returns 200 to keep nginx/Angie happy.
		if rec := recover(); rec != nil {
			c.logger.Error("panic in /check, fail open", "recover", rec)
			w.WriteHeader(http.StatusOK)
		}
	}()

	apiKey := extractKey(r)
	ip := extractIP(r)

	allowed := c.limiter.Decide(r.Context(), apiKey, ip)

	// debug log per request — what was received and what was decided.
	// Off by default at info; flip --log-level debug to enable. Includes
	// raw query and full headers so we can see what nginx/Angie actually sent
	// (useful when debugging $arg_*/$request_uri behaviour in the
	// auth_request subrequest).
	c.logger.Debug("/check",
		"x_api_key", apiKey,
		"x_real_ip", ip,
		"raw_query", r.URL.RawQuery,
		"request_uri", r.URL.RequestURI(),
		"headers", r.Header,
		"allowed", allowed,
	)

	if allowed {
		w.WriteHeader(http.StatusOK)
		return
	}
	// 403 (not 429) on block — required by nginx/Angie auth_request,
	// which only forwards 2xx / 401 / 403 to the parent request and
	// converts everything else to 500 with "auth request unexpected
	// status" in the error log. The custom-body rewrite to client is
	// done by `error_page 403 = @ratelimit_*` in nginx/Angie.
	w.WriteHeader(http.StatusForbidden)
}

// extractKey resolves the api_key (or token) from the incoming request,
// trying three sources in priority order:
//
//  1. X-Api-Key header — set by nginx/Angie via `proxy_set_header X-Api-Key`,
//     historical default. Requires nginx/Angie to extract the value through a
//     working map / set, which is non-trivial with auth_request because
//     $arg_* in the subrequest is empty (nginx#761).
//  2. Query parameter ?api_key= or ?token= on /check itself — set by
//     `proxy_pass http://ratelimiter/check?api_key=$client_key`.
//  3. X-Original-URI header — set by nginx/Angie via
//     `proxy_set_header X-Original-URI $request_uri`. We parse it
//     server-side and pull api_key/token out. This is the most robust
//     option because $request_uri *is* preserved in the auth_request
//     subrequest (other built-in vars like $args/$arg_* aren't).
//
// extractIP returns X-Real-IP only if it parses as a valid IPv4 or IPv6
// address. Garbage / spoofed headers fall back to empty so they never end
// up as a counter-map key (and so per-IP limits can't be bypassed by
// sending X-Real-IP: nonsense). Defense in depth: in the documented Angie
// config $remote_addr overrides whatever the client supplied, but a
// misconfig that forwards the raw header would otherwise be silently
// abused.
func extractIP(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("X-Real-IP"))
	if v == "" {
		return ""
	}
	if net.ParseIP(v) == nil {
		return ""
	}
	return v
}

// All three are tried so that any of these nginx/Angie config styles works.
func extractKey(r *http.Request) string {
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return v
	}
	q := r.URL.Query()
	if v := q.Get("api_key"); v != "" {
		return v
	}
	if v := q.Get("token"); v != "" {
		return v
	}
	if uri := r.Header.Get("X-Original-URI"); uri != "" {
		if u, err := url.Parse(uri); err == nil {
			uq := u.Query()
			if v := uq.Get("api_key"); v != "" {
				return v
			}
			if v := uq.Get("token"); v != "" {
				return v
			}
		}
	}
	return ""
}
