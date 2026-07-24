[![Website](https://img.shields.io/badge/pushward.app-5B4FE5?style=for-the-badge&logo=safari&logoColor=white)](https://pushward.app)
[![App Store](https://img.shields.io/badge/App_Store-Download-0D96F6?style=for-the-badge&logo=apple&logoColor=white)](https://apps.apple.com/app/id6759689999)

# PushWard Integrations

[![CI/CD GitHub](https://github.com/mac-lucky/pushward-integrations/actions/workflows/github-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/github-ci-cd.yml)
[![CI/CD SABnzbd](https://github.com/mac-lucky/pushward-integrations/actions/workflows/sabnzbd-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/sabnzbd-ci-cd.yml)
[![CI/CD BambuLab](https://github.com/mac-lucky/pushward-integrations/actions/workflows/bambulab-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/bambulab-ci-cd.yml)
[![CI/CD Grafana](https://github.com/mac-lucky/pushward-integrations/actions/workflows/grafana-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/grafana-ci-cd.yml)
[![CI/CD Relay](https://github.com/mac-lucky/pushward-integrations/actions/workflows/relay-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/relay-ci-cd.yml)
[![golangci-lint](https://github.com/mac-lucky/pushward-integrations/actions/workflows/golangci-lint.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/golangci-lint.yml)

Turn events from the services you already run — GitHub Actions, SABnzbd, a Bambu Lab printer, Grafana, and ~19 self-hosted apps behind the relay — into real-time **PushWard Live Activities, widgets, and push notifications** on your iPhone (Dynamic Island + Lock Screen). This is a Go [workspace](https://go.dev/ref/mod#workspaces) of small "bridge" programs, each shipped as its own Docker image.

> **New to PushWard?** PushWard is a push-notification platform whose iOS app renders live, updating Live Activities and widgets from any source. Learn more at **[pushward.app](https://pushward.app)** and get the iOS app on the **[App Store](https://apps.apple.com/app/id6759689999)**.

## How it works

Each bridge watches or receives events from one external service and calls the public **pushward-server** REST API. The server pushes to Apple (APNs), which delivers a Live Activity, widget update, or notification to the PushWard iOS app.

```
external service ─▶ bridge ─▶ pushward-server REST API ─▶ APNs ─▶ PushWard iOS app
  (event / webhook)             POST/PATCH /activities          (Live Activity,
                                POST /widgets                    widget, or
                                POST /notifications              notification)
```

Two shapes of bridge live here:

- **Standalone (single-tenant)** — `bambulab`, `github`, `grafana`, `sabnzbd`. Each container holds **one** PushWard integration key (`hlk_...`) and serves one account.
- **Relay (multi-tenant)** — `relay` is a single binary backed by PostgreSQL that fans out to many providers. It carries **no** key in config; instead it reads a per-request `hlk_` key from each webhook's `Authorization` header, so one deployment serves many tenants.

## Bridges

Each bridge has its own README with full configuration and per-event behavior.

| Bridge | What it pushes | Inbound port | Docker image (GHCR) |
|---|---|---|---|
| [bambulab](./bambulab/) | Bambu Lab 3D-print progress via local MQTT (TLS) | — (outbound only) | `ghcr.io/mac-lucky/pushward-bambulab` |
| [github](./github/) | GitHub Actions workflow-run CI/CD progress (poller) | — (outbound only) | `ghcr.io/mac-lucky/pushward-github` |
| [grafana](./grafana/) | Grafana alert timelines with Prometheus/VictoriaMetrics history + PromQL-polled iOS widgets | 8090 | `ghcr.io/mac-lucky/pushward-grafana` |
| [sabnzbd](./sabnzbd/) | SABnzbd download + post-processing progress | 8090 | `ghcr.io/mac-lucky/pushward-sabnzbd` |
| [relay](./relay/) | Multi-tenant webhook gateway (20 routes / 16 provider modules) | 8090 (+ 9090 metrics) | `ghcr.io/mac-lucky/pushward-relay` |

Images are published to **GitHub Container Registry only** (`ghcr.io/mac-lucky/pushward-<bridge>`). A Docker Hub name is configured in CI but `push_to_dockerhub` is `false`, so no Docker Hub images are pushed.

### Relay providers

The relay registers 20 webhook routes across 16 provider modules (`starr` serves Radarr, Sonarr, and Prowlarr; `gitea` serves Gitea and Forgejo). Every route returns `200 {"status":"ok"}` and is wrapped by the middleware chain (per-IP rate limit → `hlk_` auth → per-key rate limit). Most providers create Live Activities; the exceptions are noted.

| Provider | Route | Notes |
|---|---|---|
| Grafana | `POST /grafana` | Alert firing/resolved lifecycle — sends a grouped **push notification**, not a Live Activity (fire-and-forget) |
| ArgoCD | `POST /argocd` | Sync pipeline with deep links and `sync_grace_period` |
| Radarr | `POST /radarr` | Movie grab/download/health (HTTP Basic auth) |
| Sonarr | `POST /sonarr` | TV episode grab/download/health (HTTP Basic auth) |
| Prowlarr | `POST /prowlarr` | Indexer grab, health, application-update — sends **push notifications**, not Live Activities (HTTP Basic auth) |
| Jellyfin | `POST /jellyfin` | Playback start/progress/stop, library adds, tasks, auth failures |
| Paperless-ngx | `POST /paperless` | Document consumption/processing |
| Changedetection.io | `POST /changedetection` | Page-change alert (fire-and-forget) |
| Unmanic | `POST /unmanic` | Transcode task completion/failure (Apprise `json://`) |
| Bazarr | `POST /bazarr` | Subtitle downloaded/upgraded — sends a **push notification**, not a Live Activity (HTTP Basic auth) |
| Proxmox VE | `POST /proxmox` | Backup, replication, fencing, package-update events |
| Overseerr / Jellyseerr | `POST /overseerr` | Media request lifecycle (pending → approved → available) |
| Uptime Kuma | `POST /uptimekuma` | Monitor up/down/maintenance changes (alert) |
| Gatus | `POST /gatus` | Endpoint health status changes (alert) |
| Backrest | `POST /backrest` | Backup/prune/check/forget operations |
| Gitea | `POST /gitea` | Gitea Actions workflow-run build progress (steps) |
| Forgejo | `POST /forgejo` | Forgejo Actions run result (generic) |
| Komodo | `POST /komodo` | Resolvable conditions (server + swarm health) as Live Activities, other alerts as push notifications (HTTP Basic auth via URL userinfo) |
| TrueNAS | `POST /truenas/v2/alerts`, `DELETE /truenas/v2/alerts/{id}` | OpsGenie-compatible alert open/clear (GenieKey auth) |

Auth styles differ by what each service's webhook UI allows: most providers accept `Authorization: Bearer hlk_...`; Radarr, Sonarr, Prowlarr, Bazarr, and Komodo use HTTP Basic Auth with the `hlk_` key as the **password** (username ignored); TrueNAS uses the OpsGenie `GenieKey` scheme. See the [relay README](./relay/) for per-provider setup snippets.

## Prerequisites

- A reachable **PushWard server** — the public base is `https://api.pushward.app`.
- A **PushWard integration key** (`hlk_` prefix), created in the PushWard iOS app under Settings → Integration Keys. Publishing widgets (the `grafana` bridge) needs a key with the `widgets` scope.
- The **PushWard iOS app** installed and subscribed to the activity/widget slugs the bridge produces.
- The **backend each bridge talks to**: a GitHub token (github), a SABnzbd instance (sabnzbd), a Bambu Lab printer on the LAN (bambulab), a Prometheus/VictoriaMetrics endpoint (grafana), or PostgreSQL plus the external services' webhooks (relay).

## Installation

Run a published image, mounting your config at `/config/config.yml` (the container's default `-config` path). Start from each bridge's `config.example.yml`.

```bash
# Standalone bridge (sabnzbd shown; bambulab/grafana/github are analogous)
docker run -p 8090:8090 \
  -v ./config.yml:/config/config.yml:ro \
  ghcr.io/mac-lucky/pushward-sabnzbd:latest
```

All settings can also come from `PUSHWARD_*` environment variables (handy for `docker run -e` or Helm), which override the YAML file:

```bash
docker run \
  -e PUSHWARD_URL=https://api.pushward.app \
  -e PUSHWARD_API_KEY=YOUR_API_KEY \
  -e PUSHWARD_GITHUB_TOKEN=YOUR_GH_TOKEN \
  -e PUSHWARD_GITHUB_OWNER=your-username \
  ghcr.io/mac-lucky/pushward-github:latest
```

The relay additionally needs a PostgreSQL DSN and exposes a second (internal-only) metrics port:

```bash
docker run -p 8090:8090 -p 9090:9090 \
  -v ./config.yml:/config/config.yml:ro \
  -e PUSHWARD_URL=https://api.pushward.app \
  -e PUSHWARD_DATABASE_DSN='postgres://USER:PASS@HOST:5432/DB?sslmode=disable' \
  ghcr.io/mac-lucky/pushward-relay:latest
```

## Configuration

Configuration is layered: a YAML file (optional — a missing file is tolerated and the bridge runs from defaults + env) overlaid by `PUSHWARD_*` environment variables. **Environment variables always win.** Every bridge shares the `pushward.*` block below; bridge-specific keys (`github.*`, `sabnzbd.*`, `bambulab.*`, `metrics.*`, `providers.*`, …) live in each bridge's README and `config.example.yml`.

### Shared `pushward.*` (standalone bridges)

| Env variable | Config key | Description | Required |
|---|---|---|---|
| `PUSHWARD_URL` | `pushward.url` | PushWard server base URL, e.g. `https://api.pushward.app` | Yes |
| `PUSHWARD_API_KEY` | `pushward.api_key` | Integration key (`hlk_` prefix). Standalone bridges only — the relay uses per-request keys instead | Yes (standalone) |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Activity priority, validated `0-10` | No (varies per bridge) |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | Server `ended_ttl`: how long an ended activity lingers before cleanup | No (`15m`) |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Server `stale_ttl`: auto-end an un-updated activity | No (varies) |
| `PUSHWARD_END_DELAY` | `pushward.end_delay` | Two-phase end: delay before the final ONGOING frame | No (`5s`) |
| `PUSHWARD_END_DISPLAY_TIME` | `pushward.end_display_time` | Two-phase end: how long the final frame shows before ENDED | No (`4s`) |
| `PUSHWARD_SERVER_ADDRESS` | `server.address` | HTTP listen address (webhook bridges + relay) | No (`:8090`) |

### Relay-only essentials

The relay has **no** `pushward.api_key` (it extracts a `hlk_` key per request). It requires a database and runs a separate internal metrics server.

| Env variable | Config key | Description | Required |
|---|---|---|---|
| `PUSHWARD_URL` | — (or `-pushward-url` flag) | PushWard server base URL | Yes |
| `PUSHWARD_DATABASE_DSN` | `database.dsn` | PostgreSQL DSN for the state store. Startup fails if empty | Yes |
| `PUSHWARD_DATABASE_PASSWORD_FILE` | `database.password_file` | File holding the DB password (overrides the DSN password; live-rotated via fsnotify) | No |
| `PUSHWARD_SERVER_METRICS_ADDRESS` | `server.metrics_address` | Internal Prometheus metrics listener; must differ from `server.address` | No (`:9090`) |
| `PUSHWARD_TRUSTED_PROXY_CIDRS` | `trusted_proxy_cidrs` | CIDRs of trusted reverse proxies so `CF-Connecting-IP`/`X-Forwarded-For` is honored for per-IP rate limiting | No |
| `PUSHWARD_STARR_MODE` | `providers.starr.mode` | Radarr/Sonarr routing: `activity` (default), `notify`, or `smart` | No |

Per-provider knobs (`providers.<name>.enabled`, `priority`, `cleanup_delay`, `stale_timeout`, `end_delay`, `end_display_time`) plus the circuit-breaker and OpenTelemetry blocks are documented in the [relay README](./relay/) and [`relay/config.example.yml`](./relay/config.example.yml). All providers default to `enabled: true`.

## Endpoints

| Bridge | Endpoints |
|---|---|
| `github` | None — outbound poller, no HTTP server |
| `bambulab` | None — connects **out** to the printer over MQTT (TLS `:8883`) |
| `sabnzbd` | `POST /webhook` (optional `X-Webhook-Secret`), `GET /health`, `GET /ready` |
| `grafana` | `POST /webhook` (optional `Authorization: Bearer <webhook_token>`), `GET /health`, `GET /ready` |
| `relay` | 19 `POST` provider routes plus the TrueNAS `DELETE` (above), `GET /health`, `GET /ready`, `GET /openapi.json`, `GET /docs`; `GET /metrics` on the separate `:9090` listener |

The relay's OpenAPI 3.1 spec and interactive docs are auto-generated by [huma](https://huma.rocks/) at `/openapi.json` and `/docs`. Webhook receivers with an unset secret/token are **unauthenticated** and log a warning at startup.

## The `shared` library

[`shared/`](./shared/) is a library-only module (no `cmd/`, no Dockerfile) that every bridge imports so each one only writes its provider-specific logic. It provides:

| Package | Responsibility |
|---|---|
| `pushward` | Hand-written pushward-server REST client (activities, notifications, widgets) with retry, RFC 9457 problem parsing, and a circuit breaker |
| `config` | YAML + `PUSHWARD_*` env loading; tolerates a missing file; validates `url`/`api_key`/`priority` |
| `server` | HTTP scaffolding — `NewMux` registers `/health` and `/ready` (method-agnostic; callers use `GET`), plus graceful shutdown |
| `widgets` | Generic background widget poller publishing numeric values to the widget API (used by the `grafana` bridge today) |
| `auth` | Constant-time, fail-closed webhook header checks |
| `syncx` | Small concurrency primitives (drop counter, periodic runner, re-armable timer group) |
| `text` | Byte/size formatting, rune-safe truncation, slug + URL helpers |
| `testutil` | Mock pushward-server that validates requests against the public API contract |

The `pushward.Client` retries up to 5 attempts with exponential backoff + jitter (capped 30s) on 5xx/network errors, honors `Retry-After` on `429` (clamped to 2 minutes), and fails fast on other 4xx (except `409` limit-exceeded, surfaced as typed errors). See the [shared README](./shared/) for the full API.

## Project structure

This is a Go workspace (`go.work`, Go 1.26.5) with one shared module plus five independently-versioned bridge modules under `github.com/mac-lucky/pushward-integrations/<module>`:

```
pushward-integrations/
  go.work          # Go workspace: use ./shared ./github ./sabnzbd ./bambulab ./relay ./grafana
  shared/          # Common library (pushward client, config, server, widgets, auth, syncx, text, testutil)
  bambulab/        # Bambu Lab MQTT client — standalone bridge
  github/          # GitHub Actions poller — standalone bridge
  grafana/         # Grafana alert timelines + widgets — standalone bridge
  sabnzbd/         # SABnzbd webhook + download tracker — standalone bridge
  relay/           # Multi-tenant webhook gateway (PostgreSQL) — 16 provider modules, 20 routes
  .github/workflows/   # Per-bridge CI/CD + shared lint + release orchestrator
```

> Note: a few stale compiled binaries (`pushward-github`, `pushward-sabnzbd`, `pushward-bambulab`, `pushward-grafana`, `pushward-mqtt`) are gitignored build artifacts that can show up at the repo root after a local build; they are not committed. They are artifacts, **not** modules — there is no `mqtt/` source module and no `mqtt` entry in `go.work`. MQTT support is the `bambulab` bridge.

## Development

Run all commands from the workspace root (where `go.work` lives).

```bash
# Build a bridge (pattern: go build ./<bridge>/cmd/pushward-<bridge>)
go build ./relay/cmd/pushward-relay
go build ./grafana/cmd/pushward-grafana

# Run a standalone bridge with a config file
./pushward-grafana -config grafana/config.example.yml

# Run the relay (PUSHWARD_URL + a Postgres DSN are required)
PUSHWARD_URL=https://api.pushward.app \
PUSHWARD_DATABASE_DSN='postgres://USER:PASS@HOST:5432/DB?sslmode=disable' \
  ./pushward-relay -config relay/config.example.yml

# Tests (CI runs Go tests with -race -count=1 -v)
go test ./shared/... ./github/... ./sabnzbd/... ./bambulab/... ./grafana/... ./relay/... -race -count=1 -v

# Lint (matches CI: golangci-lint v2.11.4)
golangci-lint run
```

> Relay state tests under `relay/internal/state/...` use [testcontainers-go](https://golang.testcontainers.org/) and need a running Docker daemon.

### Docker builds

The build context is the **repo root** (not the bridge directory) so the Dockerfile can `COPY shared/`. Always pass `-f <bridge>/Dockerfile .`:

```bash
docker build -f relay/Dockerfile -t pushward-relay .
docker build -f grafana/Dockerfile -t pushward-grafana .

# Pin the Go toolchain (Dockerfile ARG default is 1.26.5)
docker build --build-arg GO_VERSION=1.26.5 -f github/Dockerfile -t pushward-github .
```

Each image builds from a `golang:<ver>-alpine` builder into an `alpine:3.23` runtime, statically (`CGO_ENABLED=0`), and runs as non-root UID 1000.

## CI/CD & Releases

Every per-bridge CI and the release workflow call the reusable `mac-lucky/actions-shared-workflows/.github/workflows/go-cicd-reusable.yml@master`; the lint workflow calls `golangci-lint-reusable.yml@master`.

- **Per-bridge CI** (`<bridge>-ci-cd.yml`) is path-filtered to `<bridge>/**` and `shared/**`, so a change to `shared/` triggers all five.
- **Lint** (`golangci-lint.yml`) runs `golangci-lint` v2.11.4 across the workspace.
- **Release** (`release.yml`) fires on per-bridge tags `<bridge>/v*`, parses the bridge + version, builds that one bridge, and creates a per-bridge GitHub Release with auto-generated, categorized notes (`.github/release.yml`).

Bridges are versioned **independently**.

### Image tag channels (GHCR)

| Trigger | Tags published | Purpose |
|---|---|---|
| Pull request | _(none)_ | Tests + analysis only |
| Push to `main` | `:main`, `:main-<short-sha>` | Rolling unstable + immutable per-commit pin |
| Git tag `<bridge>/v<X.Y.Z>` | `:X.Y.Z`, `:X.Y`, `:latest` (and `:X` once `X >= 1`) | Stable release |

`:latest` moves only on a tagged release — never on a `main` push.

### Cutting a release

```bash
# Single bridge (typical bug-fix path)
git tag relay/v0.4.1
git push origin relay/v0.4.1

# Coordinated baseline across all bridges
for b in bambulab github grafana relay sabnzbd; do git tag "$b/v0.4.0"; done
git push origin bambulab/v0.4.0 github/v0.4.0 grafana/v0.4.0 relay/v0.4.0 sabnzbd/v0.4.0
```

## Server compatibility

Bridges call the public pushward-server REST surface — `POST`/`PATCH /activities`, `POST /notifications`, `POST /widgets` (with snake_case JSON bodies). The contract (paths, request/response keys, auth headers, RFC 9457 problem codes) is owned by pushward-server's `openapi.yaml`; the `shared/pushward` client is hand-written, so keep it in sync when the server contract changes. Bridges target the server API at their `MAJOR.MINOR`; a patch release (`*.*.X`) is a bridge-only fix that needs no coordinated server bump.

## Adding a new bridge

1. **Scaffold the module.** Create `<bridge>/` with `go.mod` (`module github.com/mac-lucky/pushward-integrations/<bridge>`, `replace github.com/mac-lucky/pushward-integrations/shared => ../shared`) and `cmd/pushward-<bridge>/main.go`.
2. **Register it in the workspace.** Add `use ./<bridge>` to `go.work`.
3. **Reuse `shared`.** Load config with `shared/config`, talk to the server with `shared/pushward`, and (for webhook bridges) serve `/health` + `/ready` via `shared/server.NewMux`.
4. **Add a `config.example.yml`** with the `pushward.*` block and your bridge-specific keys, and a `<bridge>/README.md`.
5. **Add a `Dockerfile`** that builds from the repo root and `COPY`s `shared/` (copy an existing bridge's Dockerfile).
6. **Wire CI/CD.** Add `.github/workflows/<bridge>-ci-cd.yml` (path-filtered to `<bridge>/**` and `shared/**`) and a `<bridge>` job + tag pattern `<bridge>/v*` in `release.yml`.

To add a **provider to the relay** instead, see the "Adding a New Relay Provider" guide in [`CLAUDE.md`](./CLAUDE.md) and the [relay README](./relay/).

## Troubleshooting

Bridges log structured JSON to stdout — `docker logs -f <container>` (or `kubectl logs`) is your first stop.

| Symptom | Likely cause / fix |
|---|---|
| `401`/`403` from the server | Wrong or scope-limited `hlk_` key. Publishing widgets needs a key with the `widgets` scope |
| Nothing appears on iPhone | Wrong `PUSHWARD_URL`, or the iOS app isn't subscribed to the activity/widget slug the bridge produces |
| "webhook is unauthenticated" warning | `sabnzbd.webhook_secret` / grafana `webhook_token` is unset — set one and configure the matching header on the sender |
| Relay won't start | `PUSHWARD_DATABASE_DSN` missing, or `server.metrics_address` equals `server.address` (they must differ) |
| Relay rate-limits real traffic to one bucket | Set `trusted_proxy_cidrs` so the relay trusts your proxy's forwarded-IP headers |
| bambulab can't connect | Printer powered off (the bridge retries every 30s), wrong access code, or a cert-fingerprint mismatch after the printer regenerated its cert |
| `docker build` fails on `COPY shared/` | Build context must be the repo root: `docker build -f <bridge>/Dockerfile .` |

## Requirements

- Go 1.26.5 (workspace toolchain) and Docker for builds.
- A running PushWard server, an `hlk_` integration key, the PushWard iOS app, and the backend each bridge integrates with (see [Prerequisites](#prerequisites)).
