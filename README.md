# epmon

[![CI](https://github.com/epmon-dev/epmon/actions/workflows/ci.yml/badge.svg)](https://github.com/epmon-dev/epmon/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/epmon-dev/epmon)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

A watchdog for your HTTP services. List endpoints in YAML, get probing,
history, incidents and a documented JSON API — one binary, one database
file, no accounts, no SaaS.

- **Probe anything HTTP** — per-service interval, timeout, expected statuses,
  headers, body matching, self-signed TLS opt-in.
- **Honest history** — every probe stored; days without data report `null`,
  never fake green; flap-tolerant day rollups.
- **Incident log included** — open, narrate and resolve incidents via API.
- **Deploy-ready** — bearer auth, rate limits, Prometheus metrics, readiness
  probe, versioned API with an embedded OpenAPI contract.

## Quick start

```sh
cp config.example.yaml config.yaml   # point it at your endpoints
go run ./cmd/epmon -config config.yaml
```

```sh
$ curl -s localhost:8080/api/v1/status | jq .
{
  "checked_at": 1789599577,
  "overall": "operational",
  "services": [
    {
      "id": "website",
      "name": "Website",
      "url": "https://example.com",
      "up": true,
      "latency_ms": 120,
      "status_code": 200,
      "checked_at": 1789599576
    }
  ]
}
```

With Docker:

```sh
mkdir data && cp config.example.yaml data/config.yaml  # then edit it
docker compose up --build -d
curl localhost:8080/healthz   # {"ok":true}
```

Stamp release metadata into the image so it never reports `dev`:

```sh
VERSION=$(git describe --tags --always --dirty) \
COMMIT=$(git rev-parse --short HEAD) \
DATE=$(date -u +%FT%TZ) docker compose up --build -d
docker exec epmon /epmon version   # epmon <version> (commit <sha>, built <date>, ...)
```

Interactive API reference lives at `/docs` once it's running; the raw
contract at `/api/v1/openapi.yaml` (or `.json`).

## A full round trip

Probes run on their own — here's the human side:

```sh
# something breaks: open an incident (needs an API key, see below)
curl -s -X POST localhost:8080/api/v1/incidents \
  -H "Authorization: Bearer $EPMON_API_KEY" \
  -d '{"service_id":"api","title":"Elevated latency","severity":"minor"}' | jq .
# {"id":1,...,"state":"investigating","updates":[]}

# narrate as you work
curl -s -X POST localhost:8080/api/v1/incidents/1/updates \
  -H "Authorization: Bearer $EPMON_API_KEY" \
  -d '{"text":"Slow query found, index added."}' -o /dev/null -w "%{http_code}\n"
# 201

# fixed: resolve it
curl -s -X PATCH localhost:8080/api/v1/incidents/1 \
  -H "Authorization: Bearer $EPMON_API_KEY" \
  -d '{"state":"resolved"}' | jq .state
# "resolved"

# history for the status page: buckets plus uptime over the window
curl -s "localhost:8080/api/v1/services/api/history?days=90" | jq '{uptime_pct, days}'
# {"uptime_pct": 99.97, "days": 90}
```

## Configuration

Format follows the extension (`.yaml`/`.yml` vs `.json`); only
`${VAR}` (or `${VAR:-fallback}`) expands from the environment — keep
tokens out of the file. `$$` is a literal `$`; a bare `$VAR` is left
untouched, so a literal `$EPMON_API_KEY` as a key would be guessable —
always use braces.

```yaml
server:
  addr: ":8080"
  api_keys: ["${EPMON_API_KEY}"]  # writes need Bearer; empty = off (dev only)
  rate_limit_rpm: 120                # per client IP; 429 + Retry-After past it
  rate_limit_burst: 120
  trust_proxy: false                 # true only behind a sanitizing proxy
  cors_allowed_origins: []           # e.g. ["https://status.example.com"]
  max_body_bytes: 1048576
  # tls_cert: "/data/tls/cert.pem"   # or terminate TLS at your proxy
  # tls_key: "/data/tls/key.pem"

database:
  driver: sqlite          # adapter name; postgres later without touching callers
  dsn: "epmon.db"      # adapter connection string (":memory:" = ephemeral)
  retention_days: 90

probes:                   # defaults; every field overridable per service
  default_interval: 60s
  default_timeout: 10s
  failure_threshold: 1

services:
  - id: website           # required, unique; name defaults to id
    name: Website
    url: https://example.com
    body_contains: "Example"   # optional: substring the body must contain
  - id: api
    name: Public API
    url: https://api.example.com/readyz
    interval: 30s
    expect_status: [200]
    headers:
      Authorization: "Bearer ${TOKEN}"
    # insecure_skip_verify: true   # only for boxes you own
```

A probe is **up** when it answers within `timeout` with a status in
`expect_status` (after redirects) and — if set — the body contains
`body_contains`. Anything else stores `up: false` with a short error
(`transport: …`, `status: got 500`, `body: …`).

Config location, in order: `--config <path>`, `EPMON_CONFIG`,
`./epmon.yaml`, `/etc/epmon/epmon.yaml`, then `config.yaml`.
Exit codes: `0` ok, `1` config error, `2` storage error, `3` listen/bind
error, `4` failed `healthcheck`, `64` usage error.

## API (`/api/v1`)

Full contract with schemas: `/docs`, or raw at `/api/v1/openapi.yaml`.

| Method | Path | Notes |
|---|---|---|
| GET | `/healthz` | readiness: 200 only while the DB answers, else 503 |
| GET | `/api/v1/status` | `overall` (`operational`\|`partial_outage`\|`unknown`) + last check per service |
| GET | `/api/v1/services` | same per-service states as a list |
| GET | `/api/v1/services/{id}/history?days=90&limit=100` | daily buckets (1–365), `uptime_pct`, newest raw checks (1–500) |
| GET | `/api/v1/incidents[?state=][?service=]` | newest first, update threads embedded |
| POST | `/api/v1/incidents` | `{service_id?, title, severity?}` → 201 + `Location` |
| GET | `/api/v1/incidents/{id}` | single incident with updates |
| PATCH | `/api/v1/incidents/{id}` | `{title?, severity?, state?}` |
| POST | `/api/v1/incidents/{id}/updates` | `{text}` → 201 + `Location` |

Conventions: single resources are bare objects, collections are
`{"<name>": [...], "total": n}`, every error — including unknown routes — is
`{"error": {"code": "<bad_request|unauthorized|too_large|rate_limited|not_found|internal|unavailable>", "message": "…"}}`.
Trailing slashes tolerated; 201s carry `Location`. Breaking changes ship as
`/api/v2` alongside v1, never over it.

History rule, shared with status-page frontends: a day is down only when
≥10% of its probes failed (flap tolerance); probeless days report
`up: null` instead of fake green.

## Storage

`driver` + `dsn` select a registered adapter; callers only see the
`store.Store` interface. SQLite ships by default (pure Go, no cgo, one
file). Inside: `checks` holds every probe, `incidents` + `incident_updates`
the manual log; buckets derive on read and checks older than
`retention_days` are purged on boot and daily. Roughly 130k small rows per
service at one probe/minute over 90 days.

## Production checklist

- **Auth:** set `server.api_keys` from env. Writes need
  `Authorization: Bearer <key>`; reads stay public. Blank entries (unset
  `${VAR}`) are dropped, never treated as keys.
- **Rate limiting:** per-IP token bucket; 429 + `Retry-After` past the
  budget. `trust_proxy: true` only behind a proxy that sanitizes
  `X-Forwarded-For`.
- **TLS:** `tls_cert`/`tls_key`, or terminate at Caddy/Traefik. The image
  exposes plain HTTP on 8080.
- **Health & metrics:** point probes at `/healthz` (`HEALTHCHECK` is baked
  into the image); scrape `/metrics` and alert on
  `epmon_service_up == 0`.
- **Data:** back up the SQLite file with the online backup — never a
  plain `cp` of the live file, which can capture a torn write:
  `sqlite3 /data/epmon.db ".backup '/backup/epmon-$(date -u +%FT%TZ).db'"`
  (then `PRAGMA integrity_check` on restores), or mount the volume into
  your backup job and back up from there.

## Metrics (`/metrics`, Prometheus text format)

Scrape it like any Prometheus target. Per-service series carry a
`service="<id>"` label; names and labels are compatibility surface and
won't be renamed.

| Metric | Kind | Use it for |
|---|---|---|
| `epmon_service_up` | gauge | Alerting: `epmon_service_up == 0` pages. State, not last probe. |
| `epmon_probe_total{result="success"\|"failure"}` | counter | Raw outcome rates; burn-rate alerts. |
| `epmon_probe_duration_seconds` | histogram | Latency: `histogram_quantile(0.99, sum(rate(epmon_probe_duration_seconds_bucket[5m])) by (service, le))` for p99 degradation and slow drift. Buckets: 5ms…10s. |
| `epmon_probe_skipped_total` | counter | Reserved: overlap-guard skips (no guard exists yet; always zero). |
| `epmon_store_queue_depth`, `epmon_store_cmd_queue_depth` | gauges | Reserved: internal backpressure hooks for a future store actor; always zero. |
| `epmon_store_dropped_total`, `epmon_store_write_timeout_total` | counters | Reserved: unwritable today, so permanently zero — do not alert on them. |
| `epmon_service_history_migrated_total{result="ok"\|"no_match"}` | counter | Reserved: alias-migration outcomes (no migration path exists yet). |
| `epmon_config_reload_total{result="ok"\|"error"}` | counter | Reserved: config reload outcomes (no reload path exists yet). |
| `epmon_uptime_seconds`, `epmon_build_info` | gauge | Process age and build identity (`version`/`commit` from release ldflags). |

## Development

```
cmd/epmon/          binary: load config → open store → schedule → serve
internal/config/       YAML/JSON loader, defaults, validation (+tests)
internal/store/        ports: domain types + Check/Incident/Service interfaces
internal/store/sqlite/ SQLite adapter — the only package owning SQL (+tests)
internal/prober/       one HTTP check (+tests)
internal/scheduler/    per-service tick loops, graceful shutdown
internal/api/          handlers, middleware, embedded OpenAPI (+tests)
internal/metrics/      Prometheus exposition (+tests)
```

```sh
go build ./... && go vet ./... && go test ./...
```

All green with no external services (tests use `httptest` and temp-file
databases). Swapping databases means adding a sibling of
`internal/store/sqlite` behind the same ports — `scheduler`, `api` and
`cmd` only ever see interfaces, and failures surface as
`store.ErrNotFound`, never as driver types.
