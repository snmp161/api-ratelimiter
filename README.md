# api-ratelimiter

*Languages: **English** · [Русский](README.ru.md)*

Rate limiting service for an API server. Runs as a helper HTTP service
that nginx/Angie calls via `auth_request`. Does not parse the protocol,
does not build business responses — replies `200 OK` or `403 Forbidden`
with an empty body (403, not 429, because nginx/Angie's `auth_request`
only forwards 2xx / 401 / 403 to the parent — anything else becomes 500).
Response customisation is done on the nginx/Angie side via `error_page`.

Full specification — [`docs/specification.md`](docs/specification.md) (Russian).

## What's inside

- HTTP endpoint `GET /check` — the only entry point nginx/Angie calls. Reads
  `X-Api-Key` and `X-Real-IP`, returns 200 / 403.
- in-memory counters with a fixed-window algorithm and burst support.
- Redis (3 DBs): per-api-key limits, abusive api-keys, abusive IPs.
- Web admin (read-only) and Prometheus `/metrics` — both read from a
  single in-process registry.
- Graceful shutdown with a final flush of abusers into Redis.

## Architecture

```
Client ─► nginx/Angie ─auth_request─► api-ratelimiter ─► Redis (DB1/2/3)
                  │
                  ├─[200]─► PHP upstream
                  └─[403]─► error_page → 200 with custom body
```

See section 2 of `docs/specification.md` for details.

## Build

```bash
make build              # builds ./api-ratelimiter (LDFLAGS: -s -w, version from git tag)
make test               # unit tests
make test-cover         # coverage report (coverage.html)
make lint               # golangci-lint (requires golangci-lint in PATH)

# Integration tests (spawn binary + miniredis + HTTP probes; build-tag gated):
go test -tags=integration -timeout=10m ./test/integration/...
```

Requires Go **1.21+**, Redis **6.0+**.

## Run (dev)

```bash
make run
```

Brings the service up on:

- `unix:/tmp/ratelimit.sock` — `/check` for nginx/Angie
- `127.0.0.1:8080` — web admin
- `127.0.0.1:9091/metrics` — Prometheus
- expects Redis at `127.0.0.1:6379`

## Flags

All flags use the `--flag value` form (`pflag`).

| Flag                   | Default                    | Description                                                  |
|------------------------|----------------------------|--------------------------------------------------------------|
| `--listen`             | `unix:/run/ratelimit.sock` | Address for `/check` (`unix:/path` or `host:port`)           |
| `--admin-listen`       | `127.0.0.1:8080`           | Web admin                                                    |
| `--metrics-listen`     | `127.0.0.1:9091`           | Prometheus `/metrics`                                        |
| `--redis-addr`         | `127.0.0.1:6379`           | Redis address                                                |
| `--redis-password`     | `""`                       | Redis password                                               |
| `--log-level`          | `info`                     | `debug`, `info`, `warn`, `error`                             |
| `--log-format`         | `json`                     | `json` (production) or `text` (dev)                          |
| `--global-limit`       | `100`                      | Per-window limit for keys not in redisDB1 and IP-based       |
| `--burst`              | `0`                        | Extra requests above the limit per slot                      |
| `--window`             | `second`                   | Window unit: `second` or `minute`                            |
| `--cleanup-interval`   | `15m`                      | Cleanup cycle period (Go duration: `30s`, `1m`, `2h`)        |
| `--abuse-ttl`          | `15m`                      | TTL for redisDB2/redisDB3 entries (Go duration)              |
| `--abuse-multiplier`   | `10`                       | `AbuseHits` threshold = `global_limit * multiplier`          |
| `--abuse-transfer-threshold` | `3`                  | Minimum `AbuseHits` to transfer counter to Redis             |
| `--socket-mode`        | `0666`                     | File mode for unix socket from `--listen` (octal). TCP-ignored |

At startup the service validates `--burst < --global-limit * --abuse-multiplier`
and exits with an error otherwise.

## Redis — data structures

Redis must be **dedicated** to this service. DB numbers are hardcoded:

| DB    | SELECT | Contents                              |
|-------|--------|---------------------------------------|
| `DB1` | 1      | Per-api-key individual limits         |
| `DB2` | 2      | Abusive api-keys                      |
| `DB3` | 3      | Abusive IPs                           |

Example of an individual limit:

```
HSET rate:limit:abc123 created_at 1717000000 limit 500
```

See section 7 of `docs/specification.md` for details.

## Logic

- Per request the key is determined: `api_key` (if present and listed in
  DB1) → limit from DB1 (`KnownCounters` map); `api_key` not in DB1 →
  `--global-limit` (`UnknownCounters` map with `key:` prefix); empty
  `api_key` → `IP` (`UnknownCounters` with `ip:` prefix).
- In each slot `WindowCount` is incremented unconditionally. Decision:
  `WindowCount > limit + burst` → 429, otherwise 200 (with `BurstHits++`
  in the burst zone `limit < WindowCount ≤ limit + burst`).
- On slot change: if the previous slot's `WindowCount > limit` →
  `ViolationHits++` (Known), or if `> limit * multiplier` →
  `AbuseHits++` (Unknown). Then `WindowCount` resets.
- Transfer to DB2/DB3 happens **only in the cleanup cycle**, not on the
  hot path. `KnownCounters` are never transferred — violations are
  visible on `/limits` and in metrics.
- Redis unreachable → `api_key` is treated as "not in DB1", routed to
  `UnknownCounters` with the global limit. Reconnect happens
  automatically through go-redis' connection pool.
- A panic inside `/check` → `200 OK` (fail open).

## Web admin

`http://<admin-listen>/`:

- `/` — status, flags, metrics (the same data as `/metrics`)
- `/limits` — individual limits from DB1, with counter columns from
  `KnownCounters` (in-memory)
- `/abuse/keys` — abusers from DB2
- `/abuse/ips` — abusers from DB3

No authentication — proxy through nginx/Angie if you need to gate access.

## Metrics

All metrics live in the Prometheus registry, exposed both on `/metrics`
and through the web admin.

```
ratelimit_requests_total{result="allowed|blocked_individual|blocked_global"}
# total blocked = sum(rate(ratelimit_requests_total{result=~"blocked_.*"}[5m]))
ratelimit_counters_known_active
ratelimit_counters_unknown_active
ratelimit_memory_bytes
ratelimit_cleanup_runs_total
ratelimit_cleanup_deleted_total
ratelimit_cleanup_transferred_total
ratelimit_cleanup_last_duration_seconds
ratelimit_redis_errors_total
ratelimit_redis_db{1,2,3}_keys
ratelimit_check_duration_seconds  # histogram
```

Counter values are cumulative since process start. Use Prometheus
`rate()`/`increase()` for rates.

## Deploy

The systemd unit and a sample nginx/Angie config are described in sections 12
and 13 of `docs/specification.md`. A ready-to-use nginx/Angie config lives in
[`configs/nginx.example.conf`](configs/nginx.example.conf). The `.deb` package
(`packaging/`) installs the systemd unit to
`/lib/systemd/system/api-ratelimiter.service` and the binary to
`/usr/bin/api-ratelimiter`. For ad-hoc installs from source:

```bash
sudo make install      # → /usr/local/bin/api-ratelimiter
```

The version embedded in the binary comes from
`git describe --tags --always --dirty`. When built outside a repository,
it falls back to `dev`.

## Project layout

```
api-ratelimiter/
├── cmd/api-ratelimiter/main.go      # entry point
├── internal/
│   ├── config/                  # flags, validation
│   ├── counter/                 # KnownCounters, UnknownCounters
│   ├── limiter/                 # routing and decision
│   ├── handler/                 # HTTP /check
│   ├── cleanup/                 # cleanup + transfer to DB2/DB3
│   ├── store/                   # Redis (3 DBs)
│   ├── admin/                   # web admin + html templates
│   └── metrics/                 # Prometheus registry
├── configs/                     # sample nginx/Angie config + systemd unit
├── packaging/                   # nfpm.yaml + .deb install scripts
├── docs/specification.md        # full specification (Russian)
├── .github/workflows/           # CI / release pipelines
└── Makefile
```

## Limitations

- On restart, in-memory counters and Prometheus counters are reset. Data
  in Redis is preserved.
- Writes to DB2/DB3 happen once per `--cleanup-interval`. This is an
  intentional trade-off — only systematic abusers end up in the database.
- Horizontal scaling without shared state: counters are independent per
  instance. DB `0` is reserved for future Redis-backed counters.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
