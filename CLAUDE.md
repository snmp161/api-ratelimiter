# CLAUDE.md

Orientation for Claude (or any AI assistant) opening this repo. Read this
**before** touching code — saves rediscovering invariants from `git log`.

## TL;DR

**api-ratelimiter** is a single-binary Go service that nginx/Angie calls
via `auth_request` to decide whether to allow or rate-limit each API
request. It returns `200` (allow) or `403` (block) with an empty body;
nginx rewrites the 403 into a protocol-specific 200 with a custom body
via `error_page`.

- **Authoritative spec:** [`docs/specification.md`](docs/specification.md) — in Russian, ~50 sections, the source of truth for behaviour
- **Sample reverse-proxy config:** [`configs/nginx.example.conf`](configs/nginx.example.conf)
- **License:** Apache 2.0; every `.go` file starts with `// SPDX-License-Identifier: Apache-2.0`

## Tech stack

- Go 1.22 (module name: `api-ratelimiter`)
- Redis 6+ with three logical DBs hardcoded (`store/redis.go`):
  - `DB1` — per-api-key limits (operator-managed)
  - `DB2` — abusive api-keys (auto-populated by cleanup)
  - `DB3` — abusive IPs (auto-populated by cleanup)
- nginx/Angie `auth_request` module
- `prometheus/client_golang` for metrics, `pflag` for flags, `log/slog`
- `miniredis` (in-process Redis) for `store` + `admin` unit tests; no
  Docker required.

## Layout

```
cmd/api-ratelimiter/main.go      — wiring + signal handling + graceful shutdown
internal/config/                  — flag definitions, validation
internal/counter/                 — KnownMap (DB1-known) + UnknownMap (fallback)
internal/limiter/                 — route to the right map + record + decide
internal/handler/                 — GET /check; extract api_key / IP
internal/cleanup/                 — periodic GC + abuse transfer to DB2/DB3
internal/store/                   — wraps three go-redis clients + health-state tracking
internal/admin/                   — read-only web UI + Delete/Purge actions, CSRF, html/template
internal/metrics/                 — Prometheus registry (single source for /metrics and /admin)
configs/nginx.example.conf        — sample upstream config with backup-stub & bypass map
configs/api-ratelimiter.service   — systemd unit
packaging/nfpm.yaml + scripts/    — .deb packaging via nfpm
docs/specification.md             — full spec (Russian)
.github/workflows/{ci,release}.yml — CI/CD; tag v* triggers release
```

## Build / test / lint

```bash
make build       # ./api-ratelimiter
make test        # go test ./...
make test-cover  # coverage.html
make lint        # golangci-lint run ./...
```

CI runs the same with `-race`, `-coverpkg=./...`, and golangci-lint
v1.59. Match locally before committing. **All tests must be green
under `-race`.**

## Project-specific invariants (don't break)

1. **Block returns 403, not 429.** `auth_request` only forwards 2xx /
   401 / 403 to the parent; anything else becomes 500. See
   `handler/check.go` and the named `error_page 403 = @ratelimit_*`
   redirects in the nginx example.
2. **Fail-open on missing key/IP.** When both api_key and X-Real-IP are
   empty, `limiter.Decide` returns true (allow). This is "fail open
   without key" — keyless service paths must still pass through.
3. **Fail-open on Redis errors.** `LookupLimit` failures route the
   request to `UnknownCounters` with the global limit. Don't make
   Redis a hard dependency on the hot path.
4. **403 panic safety net.** `/check` is wrapped in `defer/recover` and
   returns 200 on panic — nginx never gets a 5xx from us. Don't remove.
5. **`X-Real-IP` is validated** with `net.ParseIP` (`handler/check.go`).
   Don't take the raw header as a counter-map key.
6. **Cleanup `Snapshot`→`Delete` race.** Use `DeleteIfInactive` (atomic
   under the lock) — never `Delete` based on a stale snapshot. For
   abuse upserts, re-read live state via `Get` before transfer.
7. **Inactivity threshold is 2×window**, not 1×. Trickle traffic
   (one request per ~window/2 seconds) would otherwise re-create the
   counter from zero every cleanup cycle.
8. **CSRF token on every admin POST.** `admin.checkCSRF` uses
   `subtle.ConstantTimeCompare`. The token is process-scoped (rotates
   on restart). Hidden field is rendered by templates.
9. **Redis health is tracked centrally in `store.Store`.** Logs only
   transitions (up↔down), not every error. `cleanup.runUnknown`
   checks `s.IsHealthy()` before attempting upserts. Don't add hot-path
   warn-logs for Redis failures elsewhere.
10. **Sort with explicit tiebreaker.** Snapshot iterates a Go map
    (randomised). `sort.Slice` without a `Key` tiebreaker → page reloads
    show different orders. See `admin/admin.go::topKnown`/`topUnknown`.

## What is and isn't the project name

- **Project name (prose, UI titles, package descriptions):** `api-ratelimiter`
- **Technical identifiers — same name now:** Go module, binary,
  systemd unit, nfpm package, paths, nginx upstream blocks.
- **NOT the project name — DON'T rename these:**
  - `ratelimit.sock` — fixed socket filename inside the runtime dir
  - `$ratelimit_upstream` — nginx variable
  - `ratelimit-bypass.log` — stub access-log path
  - `@ratelimit_*` — nginx named locations
  - `ratelimit_requests_total`, `ratelimit_*` — Prometheus metric prefix
  - `rate:limit:*`, `rate:abuse:*` — Redis key prefixes

When doing bulk renames: prefer per-target sed over `s/ratelimiter/x/g`
to avoid double-applications (see commit `f7560cf` for the lesson).

## Test conventions

- **Counter / cleanup tests** use a fake clock (`type clk struct{ t
  time.Time }`) and a `*counter.Known/UnknownMap` constructed with that
  clock — never real `time.Now`.
- **Store tests** use `miniredis.RunT(t)`; pass its `.Addr()` to
  `store.New(addr, "", nil)`. `nil` logger is fine — `store.New` falls
  back to a discard handler.
- **Admin tests** wire `miniredis` + `store` + an `*admin.Server`.
  CSRF tokens for POST tests come from a real GET on a list page,
  parsed out via `csrfRe` (regex). End-to-end exercises the token
  validation.
- **Cleanup tests** use `seedAbusiveUnknown(t, m, c, key)` to drive a
  counter past the `AbuseHits` transfer threshold — don't open-code
  the 4-slot × 105-request loop.
- **`prometheus/testutil.ToFloat64`** for asserting counter
  increments in tests.
- After template-touching changes, assert `strings.HasSuffix(body,
  "</html>")` — catches mid-render aborts (see
  `TestRender_EmptyDatabaseRendersAllPages`).

## CI / release lifecycle

- Push to any branch → `ci.yml` runs lint + vet + test (with coverage)
  + build + smoke test.
- Push tag `v*` → `release.yml` builds tarball + .deb via nfpm, creates
  GitHub Release with assets + auto-generated notes.
- Concurrency: ci.yml cancels in-progress on the same ref; release.yml
  queues (never cancels — partial releases would leak artifacts).
- Smoke test accepts pflag's exit codes 0 *and* 2 (`--help` exits 2);
  anything else fails the job.

## When to ask the user before acting

- `git push` — never push without explicit consent. The repo is public.
- `git tag` + push tag — triggers a release; needs approval on the
  version.
- Force-push, `git reset --hard`, deleting branches/tags — always ask.
- Anything that would invalidate existing installs (renaming the
  systemd unit, the binary, the .deb package name) — ask first.
- Running `go get -u` / dependency bumps — ask. The project pins
  versions for a reason.

## Things to not do (lessons from history)

- Don't run `go test` / `golangci-lint` after pure markdown / nginx /
  yaml changes. The user (rightly) called this out as wasted CI/local
  cycles. Sanity-check only what's actually affected.
- Don't `git commit` without `git add -A` first when you've made
  multiple file changes — the rename-rebrand work split into two
  commits because of this (`db331b9` + `f7560cf`).
- Don't hand-wave about "compatibility" or "danger" when renaming —
  for v0.x with no prod installs, most renames are mechanical, not
  risky. Be specific about the actual consequence.
- Don't use `sort.Slice` on user-visible lists without a deterministic
  secondary key — pagination jumps between reloads.

## Memory & user-personal notes

User-specific preferences (response language, tone, "don't rush"-style
guardrails) live in the per-conversation memory directory, not in this
file. CLAUDE.md is the project's collective memory; the memory dir is
the user's.
