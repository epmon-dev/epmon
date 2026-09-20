# epmon

[![CI](https://github.com/epmon-dev/epmon/actions/workflows/ci.yml/badge.svg)](https://github.com/epmon-dev/epmon/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/epmon-dev/epmon)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

A watchdog for your HTTP services. List endpoints in YAML, get probing,
history, incidents and a documented JSON API — one binary, one database
file, no accounts, no SaaS.

- **Probe anything HTTP** — per-service interval, timeout, expected statuses,
  headers, body matching, self-signed TLS opt-in.
- **Exact history** — every probe stored; days without data report `null`,
  never fake green; flap-tolerant day rollups.
- **Incident log included** — open, narrate and resolve incidents via API.
- **Status UI in the box** — the binary serves its own status page with
  90-day bars at `/`; no second deployment.
- **Deploy-ready** — bearer auth, rate limits, Prometheus metrics, readiness
  probe, versioned API with an embedded OpenAPI contract.

## Quick start

Generate a config, check it, run it:

```sh
epmon init                          # interactive walkthrough → epmon.yaml
epmon validate --config epmon.yaml  # parses + validates, no side effects
epmon run --config epmon.yaml       # probe + serve
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

Then open `http://localhost:8080/` — the status page with 90-day bars,
served by the same binary. Details in [Status UI](#status-ui).

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

## Installation

Pick one:

```sh
# from source (Go 1.25+)
go install github.com/epmon-dev/epmon/cmd/epmon@v0.1.0

# release binaries (linux/amd64+arm64, darwin/arm64, windows/amd64)
# → https://github.com/epmon-dev/epmon/releases (stamped, no build needed)

# docker
docker compose up --build -d   # see VERSION/COMMIT/DATE above for stamping
```

Or `make build` in a checkout — same flags the Dockerfile uses
(`make help` lists every target: `test`, `cross`, `lint`, `check-docs`…).

## CLI reference

```
epmon run [--config PATH]          probe + serve (default command)
epmon validate [--config PATH]     parse + validate only; exit 0/1, no side effects
epmon healthcheck --endpoint URL   GET <url>/healthz; exit 0 on 200, 4 otherwise
epmon version                      print version/commit/date; exit 0
epmon init [flags]                 scaffold a config (see below)
```

Exit codes: `0` ok, `1` config error, `2` storage error, `3` listen/bind
error, `4` failed `healthcheck`, `64` usage error.

### `epmon init`

Two modes, one code path — the output always passes `validate`:

```sh
# interactive: prompts with [defaults], empty takes them, Ctrl-C aborts
epmon init --output epmon.yaml

# scripted: same walk driven by flags (repeat --service)
epmon init --non-interactive --output epmon.yaml \
  --server-listen 127.0.0.1:8080 \
  --service 'api=https://api.example.com/healthz;interval=30s;expect=200,301' \
  --service 'web=https://example.com'
```

`--service` takes `id=url[;key=value...]` with keys `name`, `interval`,
`timeout`, `expect`, `body_contains`. Expected statuses accept every form
the loader does (`200`, `2xx`, `200,301`). Header values are
interactive-only, and `$NAME` becomes `${NAME}` so secrets stay references,
never literal tokens. Existing files are never overwritten without
`--force`; nothing is written unless the document validates.

## Configuration

Format follows the extension (`.yaml`/`.yml` vs `.json`); only
`${VAR}` (or `${VAR:-fallback}`) expands from the environment — keep
tokens out of the file. `$$` is a literal `$`; a bare `$VAR` is left
untouched, so a literal `$EPMON_API_KEY` as a key would be guessable —
always use braces.

```yaml
server:
  listen: ":8080"               # bind address; addr is the legacy alias
  api_keys: ["${EPMON_API_KEY}"]  # writes need Bearer; empty = off (dev only)
  rate_limit_rpm: 120             # per client IP; 429 + Retry-After past it
  rate_limit_burst: 120
  trust_proxy: false              # true only behind a sanitizing proxy
  cors_allowed_origins: []        # e.g. ["https://status.example.com"]
  max_body_bytes: 1048576
  status_page: {enabled: true}    # the UI at /; false restores JSON 404s
  # tls_cert: "/data/tls/cert.pem"   # or terminate TLS at your proxy
  # tls_key: "/data/tls/key.pem"

database:
  driver: sqlite          # adapter name; postgres later without touching callers
  dsn: "epmon.db"         # adapter connection string (":memory:" = ephemeral)
  retention_days: 90

probes:                   # defaults; every field overridable per service
  default_interval: 60s
  default_timeout: 10s
  failure_threshold: 1
  concurrency: 64
  max_body_bytes: 1048576

history:
  timezone: UTC             # IANA name; day buckets cut at its midnights

api:
  max_page_size: 100        # incident-list page cap (10..1000)

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
    # enabled: false               # skip probing, keep serving history
```

Key rules, enforced at load (unknown keys are errors, with file:line):

- **Service identity:** `id` matches `^[a-z0-9][a-z0-9_-]{0,63}$`, unique;
  at least one service is required. Renaming an id archives the old row
  and starts fresh history — history moves only via explicit migration.
- **Budgets:** service `interval` 5s–24h, `timeout` ≥1s and strictly below
  its interval; unset fields inherit the probe defaults; unset
  `expect_status` means `[200]`.
- **Up means:** answered within `timeout`, status in `expect_status`
  (after redirects), and — if set — body contains `body_contains`.
  Anything else stores `up: false` with a short error (`transport: …`,
  `status: got 500`, `body: …`).
- **`enabled: false`** stops probing that service; its history, incidents
  and catalog entries keep serving (see [Run modes](#run-modes)).

Config location, in order: `--config <path>`, `EPMON_CONFIG`,
`./epmon.yaml`, `/etc/epmon/epmon.yaml`, then `config.yaml`.

## Run modes

**Full (default).** `epmon run --config epmon.yaml` — probes, API, UI.
This is the mode everything else degrades from.

**Headless API.** The UI is the only optional surface:

```yaml
server:
  listen: ":8080"
  status_page: {enabled: false}
database:
  driver: sqlite
  dsn: "epmon.db"
  retention_days: 90
services:
  - id: website
    url: https://example.com
```

Probes, API, metrics and `/healthz` behave exactly as before; `/`
and friends return the JSON 404 again. Use it behind a reverse proxy
that serves its own frontend, or when the binary is a pure data plane.

**API + history, no active probing.** Mark services `enabled: false`
(either in the file or by flipping them later): the scheduler skips
them, nothing new is recorded, and the API keeps serving the catalog,
history, incidents and last-known states. Useful for a retired
environment you still want readable, or a warm standby that must not
generate traffic until you flip it back.

**Ephemeral.** `dsn: ":memory:"` — full behavior, zero disk. Probes run,
API answers, restart wipes everything. Made for CI smoke tests and
`init` output trials:

```sh
epmon init --non-interactive --output /tmp/try.yaml --db-dsn ":memory:" \
  --service 'demo=https://example.com;interval=15s'
epmon validate --config /tmp/try.yaml && epmon run --config /tmp/try.yaml
```

**Validate-only.** `epmon validate` in CI or pre-deploy hooks: parses,
substitutes env, enforces every rule above, touches no network or disk
beyond reading the file. Fails closed on unknown keys and unset
`${VAR}` (all missing vars reported at once, with file:line).

## Status UI

The binary serves its own status page — no second deployment:

| Path | What |
|---|---|
| `/` | redirects to the local core's services |
| `/local` | your services: state, latency, 90-day bars, incident feed |
| `/p/:slug`, `/s/:domain` | multi-tenant demo routes (preview/testing) |
| `/embed` | badge snippet docs; `/embed.js` is the 2KB badge script |

Bars are exact history rendered honestly: green days held up, red days
didn't, hatched days had no telemetry — never interpolated. Hover any
bar for the date, outcome and check count. Disable the whole surface
with `status_page.enabled: false` (see [Run modes](#run-modes)).

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

## API (`/api/v1`)

Full contract with schemas: `/docs`, or raw at `/api/v1/openapi.yaml`.

| Method | Path | Notes |
|---|---|---|
| GET | `/healthz` | readiness: 200 only while the DB answers, else 503 |
| GET | `/api/v1/status` | `overall` (`operational`\|`partial_outage`\|`unknown`) + last check per service |
| GET | `/api/v1/services` | same per-service states as a list |
| GET | `/api/v1/services/{id}/history?days=90&limit=100` | daily buckets (1–365), `uptime_pct`, newest raw checks (1–500) |
| GET | `/api/v1/incidents?state=&service=&limit=&offset=` | newest first, update threads embedded; `state` rejects typos with 400 |
| POST | `/api/v1/incidents` | `{service_id?, title, severity?}` → 201 + `Location` |
| GET | `/api/v1/incidents/{id}` | single incident with updates |
| PATCH | `/api/v1/incidents/{id}` | `{title?, severity?, state?}` (forward-only states) |
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
- **Upgrades:** stop the binary, replace it, start it. Schema migrations
  are forward-only and run at boot; downgrading across a migration
  boundary refuses to start — read the release notes first.
- **Config changes:** edit the file, then `epmon validate` before
  reloading the process. Unknown keys fail closed so typos can't
  silently disable a probe.

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

## Troubleshooting

**`bind: address already in use` (exit 3).** Something owns the port.
Point `server.listen` at a free one (`127.0.0.1:18082` for local trials)
— the binary exits instead of hanging, by design.

**`validate` fails on `${VAR}`.** The variable isn't set where you run
it. Export it or use `${VAR:-fallback}`. All missing vars are reported
at once, with file:line — fix them all in one pass.

**A service never leaves `unknown`.** `up: null` means zero checks have
landed: either the scheduler hasn't ticked yet (first probe fires at
boot, so wait one interval), the service is `enabled: false`, or every
probe is erroring before evaluation (check the process log for the
`transport:` reason).

**History shows `up: null` for past days.** That's the design, not data
loss: days before the first recorded check report null. Backfilling
would be fabrication.

**429s from your own monitoring.** You're polling faster than
`rate_limit_rpm` allows. Raise the budget or slow the poller; reads and
the UI share the same bucket.

**Timezone day boundaries look off.** Buckets cut at `history.timezone`
midnights (default UTC), not the viewer's. Set the IANA name you
operate in.

## FAQ

**Can it serve the API without probing?** Yes — set `enabled: false`
on every service ([Run modes](#run-modes)): no traffic generated,
catalog/history/incidents keep serving. There is no separate
API-only binary, by design: one binary, fewer moving parts.

**Why SQLite?** One file, zero cgo, online backup, and far more
write headroom than 1,000 services at 5s intervals need. A second
adapter plugs in behind `store.Store` without touching callers.

**Why is a zero-service config invalid?** A monitor watching nothing is
a scripting bug or a typo away from silence. `epmon init` won't produce
one either.

**How do I rename a service without losing history?** You don't get it
for free: the old id archives, the new one starts fresh. That's the
exact-history invariant — renames are visible by design. Declare the
rename with `aliases` so the intent is recorded alongside the service.

**Where do secrets go?** In the environment, referenced as `${VAR}`.
`epmon init` writes `$NAME` as `${NAME}` automatically and never
persists a literal token.

## Development

```
cmd/epmon/          binary: load config → open store → schedule → serve
internal/config/       YAML/JSON loader, defaults, validation (+tests)
internal/store/        ports: domain types + Check/Incident/Service interfaces
internal/store/sqlite/ SQLite adapter — the only package owning SQL (+tests)
internal/prober/       one HTTP check (+tests)
internal/scheduler/    per-service tick loops, graceful shutdown
internal/api/          handlers, middleware, embedded OpenAPI + status UI (+tests)
internal/api/webui/    vendored status SPA (refresh with `make web`)
internal/metrics/      Prometheus exposition (+tests)
scripts/               check-docs.py (README validation), refresh-webui.sh
```

```sh
make check      # fmt + vet + race tests (what CI runs)
make check-docs # every README yaml block must validate
make build      # stamped local binary
make help       # all 19 targets
```

All green with no external services (tests use `httptest` and temp-file
databases). Swapping databases means adding a sibling of
`internal/store/sqlite` behind the same ports — `scheduler`, `api` and
`cmd` only ever see interfaces, and failures surface as
`store.ErrNotFound`, never as driver types.
