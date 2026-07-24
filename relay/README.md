[![Website](https://img.shields.io/badge/pushward.app-5B4FE5?style=for-the-badge&logo=safari&logoColor=white)](https://pushward.app)
[![App Store](https://img.shields.io/badge/App_Store-Download-0D96F6?style=for-the-badge&logo=apple&logoColor=white)](https://apps.apple.com/app/id6759689999)
[![Docs](https://img.shields.io/badge/Docs-API_Reference-5B4FE5?style=for-the-badge&logo=readthedocs&logoColor=white)](https://pushward.app)

[![CI/CD Relay](https://github.com/mac-lucky/pushward-integrations/actions/workflows/relay-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/relay-ci-cd.yml)
[![Image](https://img.shields.io/badge/ghcr.io-pushward--relay-2496ED?logo=docker&logoColor=white)](https://github.com/mac-lucky/pushward-integrations/pkgs/container/pushward-relay)

# PushWard Relay

Self-hostable, multi-tenant webhook gateway that turns webhooks from your homelab and infrastructure tools — Grafana, ArgoCD, the \*arr suite, Proxmox, Jellyfin, and more — into **PushWard Live Activities and push notifications** on iPhone (Dynamic Island + Lock Screen). One binary serves many tenants: each request is authenticated by its own `hlk_` integration key, so there is **no per-service API key in config**.

> **New to PushWard?** PushWard delivers real-time Live Activities to your iPhone's Dynamic Island and Lock Screen. Learn more at [pushward.app](https://pushward.app) and download the app from the [App Store](https://apps.apple.com/app/id6759689999).

## Contents

- [How it works](#how-it-works)
- [Features](#features)
- [Prerequisites](#prerequisites)
- [Quickstart (Docker)](#quickstart-docker)
- [Configuration](#configuration)
- [Build & run from source](#build--run-from-source)
- [Endpoints](#endpoints)
- [Providers](#providers)
- [Development](#development)
- [CI/CD & Releases](#cicd--releases)
- [Server compatibility](#server-compatibility)
- [Troubleshooting](#troubleshooting)
- [Requirements & License](#requirements--license)

## How it works

```
external service ──POST /<provider>──▶ pushward-relay ──REST API──▶ pushward-server ──APNs──▶ iOS
   (hlk_ key in Authorization)        (auth + rate limit)        (api.pushward.app)        (Live Activity / push)
```

A service POSTs its native webhook to a per-provider route (e.g. `POST /grafana`). The relay extracts the tenant's `hlk_` key from the `Authorization` header, decodes the payload, maps the event to the PushWard activity lifecycle (create / update / two-phase end) or a one-shot push notification, and calls the [pushward-server](https://pushward.app) REST API. The server delivers via APNs to the PushWard iOS app. Per-tenant state (alert grouping, ArgoCD sync tracking, download dedup) is persisted in PostgreSQL with TTL cleanup.

## Features

- **Multi-tenant by design** — tenants are identified by their `hlk_` integration key, extracted from every request by shared auth middleware. No per-service key configuration; one relay serves many users.
- **20 webhook routes** across **16 configurable provider blocks** (the `starr` block serves Radarr, Sonarr, and Prowlarr; the `gitea` block serves Gitea and Forgejo). See [Providers](#providers).
- **Two-phase end lifecycle** — completion events send a final `ONGOING` update (so the result shows on the Dynamic Island), then `ENDED` after a short display delay. Used by ArgoCD, Radarr, Sonarr, Jellyfin, Paperless, Unmanic, Proxmox, Overseerr, Uptime Kuma, Gatus, Backrest, Gitea, Forgejo, Komodo, and TrueNAS. Grafana, Prowlarr, Bazarr, and Changedetection are fire-and-forget.
- **Push notifications** — one-shot APNs alerts for events that don't fit a Live Activity (Grafana alerts, Bazarr subtitle downloads, Prowlarr grabs).
- **Cross-provider notification threads** — Radarr/Sonarr/Overseerr/Jellyfin notifications about the same movie (TMDB id) or show (TVDB id) collapse into one iOS notification thread.
- **PostgreSQL state store** — persistent alert grouping, sync tracking, and download dedup with a background TTL sweep every 30s.
- **Per-tenant client pool** — LRU pool of PushWard API clients keyed by `hlk_` hash (up to 1,000 concurrent tenants), wrapped in a shared circuit breaker.
- **Dual-layer rate limiting** — per-IP (5 req/s, burst 20) and per-key (1 req/s, burst 10) token buckets.
- **Live credential rotation** — optional DB `password_file` watched via fsnotify; the connection pool resets automatically when the file changes.
- **Built-in observability** — auto-generated OpenAPI 3.1 spec (`/openapi.json`) + interactive docs (`/docs`), Prometheus `/metrics` on a separate internal listener, and optional OpenTelemetry OTLP/gRPC tracing.
- **Graceful shutdown** — flushes pending two-phase ENDED timers and waits for in-flight callbacks on SIGINT/SIGTERM.

## Prerequisites

- A running **PushWard server** (`https://api.pushward.app`, or your own deployment)
- A **PostgreSQL** database for the relay state store
- The **PushWard iOS app** ([App Store](https://apps.apple.com/app/id6759689999)) subscribed to the slugs you push to
- One **PushWard integration key** (`hlk_` prefix) per tenant — created in the PushWard app

## Quickstart (Docker)

The Docker build context **must be the repo root** so the Dockerfile can `COPY shared/` and `relay/`.

```bash
# Build (context = repo root, not the relay/ dir)
docker build -f relay/Dockerfile -t pushward-relay .

# Run (exposes the webhook server :8090 and the metrics server :9090)
docker run -p 8090:8090 -p 9090:9090 \
  -v "$(pwd)/config.yml:/config/config.yml:ro" \
  -e PUSHWARD_URL=https://api.pushward.app \
  -e PUSHWARD_DATABASE_DSN='postgres://user:pass@db:5432/pushward_relay?sslmode=disable' \
  pushward-relay
```

### Docker Compose

```yaml
services:
  pushward-relay:
    image: ghcr.io/mac-lucky/pushward-relay:latest
    ports:
      - "8090:8090"   # webhook server
      - "9090:9090"   # internal Prometheus metrics
    volumes:
      - ./config.yml:/config/config.yml:ro
    environment:
      - PUSHWARD_URL=https://api.pushward.app
      - PUSHWARD_DATABASE_DSN=postgres://user:pass@db:5432/pushward_relay?sslmode=disable
```

The image `ENTRYPOINT` is `/pushward-relay` with default `CMD ["-config", "/config/config.yml"]`, runs as non-root UID 1000, and exposes ports 8090 and 9090.

## Configuration

Settings come from a YAML config file (`-config` flag, default `config.yml`) **or** environment variables. **Environment variables override YAML.** The standardized env prefix is `PUSHWARD_*`. See [`config.example.yml`](./config.example.yml) for the full annotated example.

### Required

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_URL` | _(none)_ — also `-pushward-url` flag | PushWard server base URL the relay calls to create/update/end activities and send notifications. The `-pushward-url` flag wins over the env var. | Yes |
| `PUSHWARD_DATABASE_DSN` | `database.dsn` | PostgreSQL connection string (pgx DSN). Config load fails if empty. | Yes |

### Server & runtime

| Env Variable | Config Key | Description | Default |
|---|---|---|---|
| `PUSHWARD_SERVER_ADDRESS` | `server.address` | Listen address for the main webhook HTTP server. | `:8090` |
| `PUSHWARD_SERVER_METRICS_ADDRESS` | `server.metrics_address` | Listen address for the internal-only Prometheus metrics server (`GET /metrics`). Must differ from `server.address` or config load fails. Set empty to disable. | `:9090` |
| `PUSHWARD_DATABASE_PASSWORD_FILE` | `database.password_file` | Path to a file holding the DB password; overrides the password in the DSN and is watched via fsnotify for live rotation (pool resets on change). | _(empty)_ |
| `PUSHWARD_TRUSTED_PROXY_CIDRS` | `trusted_proxy_cidrs` | CIDRs of trusted reverse proxies. Only when `RemoteAddr` falls in one of these are `CF-Connecting-IP` / `X-Real-IP` / `X-Forwarded-For` honored for per-IP rate limiting. Comma-separated as env; a YAML list in the file. | _(empty)_ |
| _(none)_ | `circuit_breaker.threshold` | Consecutive outbound-API failures before the breaker opens. Must be `>= 1`. | `5` |
| _(none)_ | `circuit_breaker.cooldown` | How long the breaker stays open before allowing a probe. Must be `>= 1s`. | `30s` |

### Telemetry (OpenTelemetry, optional)

Tracing is fully disabled when `telemetry.endpoint` is empty.

| Env Variable | Config Key | Description | Default |
|---|---|---|---|
| `PUSHWARD_OTEL_ENDPOINT` | `telemetry.endpoint` | OTLP gRPC endpoint. Empty disables tracing entirely. | _(empty)_ |
| `PUSHWARD_OTEL_TLS_CERT_PATH` | `telemetry.tls_cert_path` | Client certificate PEM for mTLS (cert and key both required for mTLS). | _(empty)_ |
| `PUSHWARD_OTEL_TLS_KEY_PATH` | `telemetry.tls_key_path` | Client private key PEM for mTLS. | _(empty)_ |
| `PUSHWARD_OTEL_SAMPLE_RATE` | `telemetry.sample_rate` | Trace sampling rate `0.0`–`1.0`. | `1.0` |

### Provider toggles

All 16 provider blocks default to `enabled: true`. Env toggles exist **only** for `grafana`, `argocd`, `starr`, and `gitea`; every other provider can be disabled via YAML (`enabled: false`).

| Env Variable | Config Key | Description | Default |
|---|---|---|---|
| `PUSHWARD_GRAFANA_ENABLED` | `providers.grafana.enabled` | Enable/disable the Grafana provider. | `true` |
| `PUSHWARD_ARGOCD_ENABLED` | `providers.argocd.enabled` | Enable/disable the ArgoCD provider. | `true` |
| `PUSHWARD_STARR_ENABLED` | `providers.starr.enabled` | Enable/disable Radarr/Sonarr/Prowlarr. | `true` |
| `PUSHWARD_GITEA_ENABLED` | `providers.gitea.enabled` | Enable/disable the Gitea/Forgejo provider. | `true` |
| `PUSHWARD_STARR_MODE` | `providers.starr.mode` | Radarr/Sonarr routing: `activity` (default), `notify`, or `smart`. | `activity` |
| `PUSHWARD_ARGOCD_URL` | `providers.argocd.url` | ArgoCD UI base URL used to build deep links in activities. | _(empty)_ |
| `PUSHWARD_ARGOCD_SYNC_GRACE_PERIOD` | `providers.argocd.sync_grace_period` | Defers activity creation for fast syncs that complete within this window. `PUSHWARD_SYNC_GRACE_PERIOD` is a legacy fallback. | `10s` |

### Per-provider tuning (YAML only)

Each provider block accepts these keys (defaults vary per provider — see [`config.example.yml`](./config.example.yml)):

| Config Key | Description | Typical default |
|---|---|---|
| `priority` | PushWard activity priority `0`–`10`. | varies (grafana `10` compiled-in but `5` in `config.example.yml`, uptimekuma/gatus `5`, proxmox `4`, argocd `3`, changedetection/backrest `2`, most `1`) |
| `cleanup_delay` | Maps to the activity's ended TTL (how long an ended activity lingers). | `15m` |
| `stale_timeout` | State-store stale TTL. Must be `> 0` for any enabled provider — a non-positive TTL writes rows that are never cleaned up (config load fails). | varies (`24h` / `1h` / `30m`) |
| `end_delay` | Delay before the final `ONGOING` (phase-1) update; `ENDED` then follows `end_display_time` later. Unused by grafana/changedetection. | `5s` |
| `end_display_time` | How long the final completion content shows before `ENDED`. Unused by grafana/changedetection. | `4s` |

Provider-specific extras: `argocd.url`, `argocd.sync_grace_period`, `starr.mode`, `jellyfin.progress_debounce` (default `10s`), `jellyfin.pause_timeout` (default `5m`).

## Build & run from source

`pushward-relay` lives in a Go workspace (`go.work`) with a shared module. The build path differs depending on where you run it:

```bash
# From the pushward-integrations workspace root (uses go.work)
go build ./relay/cmd/pushward-relay

# From inside the relay/ directory
go build -o pushward-relay ./cmd/pushward-relay

# Run with a config file (from the workspace root)
./pushward-relay -config relay/config.example.yml

# Minimum run with env vars (no config file needed)
PUSHWARD_URL=https://api.pushward.app \
PUSHWARD_DATABASE_DSN='postgres://user:pass@localhost:5432/pushward_relay?sslmode=disable' \
./pushward-relay
```

Flags: `-config` (default `config.yml`) and `-pushward-url` (overrides `PUSHWARD_URL`).

## Endpoints

All webhook routes require an `hlk_` key (Bearer, HTTP Basic password, or the OpsGenie `GenieKey` scheme), enforce a 1 MB body limit, and return `200` with `{"status":"ok"}` on success - `401` if the key is missing, `429` when rate-limited. Requests with a missing or `text/plain` `Content-Type` are normalized to `application/json` so misconfigured senders are still accepted.

| Method | Path | Description |
|---|---|---|
| POST | `/grafana` | Grafana alert webhooks |
| POST | `/argocd` | ArgoCD sync webhooks |
| POST | `/radarr` | Radarr download/library/health webhooks |
| POST | `/sonarr` | Sonarr download/library/health webhooks |
| POST | `/prowlarr` | Prowlarr indexer grab/health/application-update webhooks |
| POST | `/bazarr` | Bazarr subtitle Apprise notifications (push) |
| POST | `/jellyfin` | Jellyfin webhook-plugin notifications |
| POST | `/paperless` | Paperless-ngx workflow webhooks |
| POST | `/changedetection` | Changedetection.io notifications |
| POST | `/unmanic` | Unmanic Apprise notifications |
| POST | `/proxmox` | Proxmox VE notification webhooks |
| POST | `/overseerr` | Overseerr/Jellyseerr request webhooks |
| POST | `/uptimekuma` | Uptime Kuma monitor webhooks |
| POST | `/gatus` | Gatus health-check alert webhooks |
| POST | `/backrest` | Backrest backup/prune/check/forget webhooks |
| POST | `/gitea` | Gitea Actions workflow_run/workflow_job webhooks |
| POST | `/forgejo` | Forgejo Actions action_run_* webhooks |
| POST | `/komodo` | Komodo Custom-alerter webhooks |
| POST | `/truenas/v2/alerts` | TrueNAS OpsGenie create-alert calls |
| DELETE | `/truenas/v2/alerts/{id}` | TrueNAS OpsGenie close-alert calls |
| GET | `/health` | Liveness — returns `ok` |
| GET | `/ready` | Readiness — `ready`, or `503` if the DB ping fails |
| GET | `/openapi.json` | Auto-generated OpenAPI 3.1 spec |
| GET | `/docs` | Interactive API docs |
| GET | `/metrics` | Prometheus metrics — served on the **separate** internal listener (`:9090`), not on `:8090` |

## Providers

| Service | Route | Auth | Output | Two-phase end |
|---|---|---|---|---|
| Grafana | `POST /grafana` | Bearer | Push notification | No (fire-and-forget) |
| ArgoCD | `POST /argocd` | Bearer | Live Activity (steps) | Yes |
| Radarr | `POST /radarr` | Basic | Live Activity (steps) + push (health) | Yes |
| Sonarr | `POST /sonarr` | Basic | Live Activity (steps) + push (health) | Yes |
| Prowlarr | `POST /prowlarr` | Basic | Push notification | No (fire-and-forget) |
| Bazarr | `POST /bazarr` | Basic | Push notification | No (fire-and-forget) |
| Jellyfin | `POST /jellyfin` | Bearer | Live Activity + push | Yes |
| Paperless-ngx | `POST /paperless` | Bearer | Live Activity | Yes |
| Changedetection.io | `POST /changedetection` | Bearer | Live Activity (alert) | No (fire-and-forget) |
| Unmanic | `POST /unmanic` | Bearer | Live Activity | Yes |
| Proxmox VE | `POST /proxmox` | Bearer | Live Activity | Yes |
| Overseerr / Jellyseerr | `POST /overseerr` | Bearer | Live Activity (steps) | Yes |
| Uptime Kuma | `POST /uptimekuma` | Bearer | Live Activity (alert) | Yes |
| Gatus | `POST /gatus` | Bearer | Live Activity (alert) | Yes |
| Backrest | `POST /backrest` | Bearer | Live Activity (steps) | Yes |
| Gitea | `POST /gitea` | Bearer | Live Activity (steps) | Yes |
| Forgejo | `POST /forgejo` | Bearer | Live Activity (generic) | Yes |
| Komodo | `POST /komodo` | Basic | Live Activity (alert) + push | Yes |
| TrueNAS | `POST /truenas/v2/alerts` · `DELETE /truenas/v2/alerts/{id}` | GenieKey | Live Activity (alert) + push | Yes |

### Authentication

Every route requires the `hlk_` integration key. The relay accepts it two ways (scheme match is case-insensitive):

- **Bearer** (default) - `Authorization: Bearer hlk_...`. Used by Grafana, ArgoCD, Jellyfin, Paperless, Changedetection, Unmanic, Proxmox, Overseerr, Uptime Kuma, Gatus, Backrest, Gitea, Forgejo.
- **HTTP Basic** - the `hlk_` key is the password (username ignored), because the sender only offers Basic Auth or a URL with userinfo. Used by **Radarr, Sonarr, Prowlarr, Bazarr, and Komodo**.
- **GenieKey** - the OpsGenie scheme (`Authorization: GenieKey hlk_...`). Used by **TrueNAS** (its OpsGenie alert service sends the key this way).

### Query parameters

Append query parameters to any webhook URL to override how the relay handles that one request. They work on every route (including the TrueNAS `DELETE`), and an explicit parameter always wins over the provider's computed value and the static config. Leave them off and behavior is byte-for-byte unchanged.

| Parameter | Values | Effect |
|---|---|---|
| `channels` | comma-separated subset of `activity`, `notification` | Restricts delivery to the listed surfaces. `channels=notification` never creates or updates a Live Activity (each event is delivered as a one-shot notification where the provider has one); `channels=activity` drops every push notification the handler would send (new and resolved) but keeps the Live Activity flow. |
| `priority` | integer `0`-`10` | Overrides the provider's `priority` config for the activity it creates. |
| `level` | `passive`, `active`, `time-sensitive`, `critical` | Overrides the interruption level of every notification the handler sends. |

An unknown `channels` value, an out-of-range or non-integer `priority`, or an invalid `level` returns `400` before the handler runs.

Example: deliver Komodo as notifications only, at priority 8, with a passive interruption level:

```
https://relay.pushward.app/komodo?channels=notification&priority=8&level=passive
```

Note the asymmetry: Live-Activity-only providers (ArgoCD, Proxmox, Backrest, Gitea/Forgejo, Jellyfin playback) have no one-shot notification to fall back to, so `channels=notification` suppresses their output entirely; notification-only providers (Grafana, Prowlarr, Bazarr) have no Live Activity, so `channels=activity` suppresses theirs.

---

### Grafana

Receives Grafana alert webhooks. Groups alerts by `alertname` into one push notification per group.

| | |
|---|---|
| Route | `POST /grafana` · Auth Bearer |
| CollapseID | `grafana-<sha256(alertname)[:12]>` (first 6 bytes = 12 hex chars) |

**Events:** `firing` → active push, `resolved` → passive push (notification `Level`). Fire-and-forget (no two-phase end). Severity is recorded only in the notification metadata — no color or icon mapping is applied.

**Setup:** In Grafana, go to **Alerts & IRM > Alerting > Notification configuration** and open the **Contact points** tab. Add a contact point with integration type **Webhook**. Set the URL to `https://relay.pushward.app/grafana`. Under *Optional settings*, set the Authorization header scheme to `Bearer` and credentials to your `hlk_` key. Adding a `severity` label (`critical`/`warning`/`info`) to alert rules records the severity in the notification metadata.

### ArgoCD

Receives ArgoCD sync webhooks via argocd-notifications. Maps sync progress to a 3-step pipeline.

| | |
|---|---|
| Route | `POST /argocd` · Template `steps` · Auth Bearer |
| Slug | `argocd-<sanitized-app-name>` |

**Events:** `sync-running` → Step 1/3 Syncing, `sync-succeeded` → Step 2/3 Rolling out, `deployed` → Step 3/3 Deployed, `sync-failed` → Sync Failed, `health-degraded` → Degraded (transient warning during rollout).

**Grace period:** `sync_grace_period` (default `10s`) defers activity creation for fast syncs that complete before the window expires, suppressing no-op notifications.

**Setup:** Configure `argocd-notifications-cm` with a webhook service pointing to `POST /argocd`, Go-templated bodies per event, and trigger expressions. Store the `hlk_` key in `argocd-notifications-secret` and reference it as `$KEY_NAME` in the `Authorization: Bearer` header. Use `oncePer: app.status.operationState.startedAt` so every sync fires all events. See the [ArgoCD webhook docs](https://argo-cd.readthedocs.io/en/stable/operator-manual/notifications/services/webhook/). The webhook body needs only: `{"app":"…","event":"…","revision":"…","repo_url":"…"}`. Set `providers.argocd.url` to build deep links.

### Radarr / Sonarr

Receives Radarr and Sonarr webhooks. Tracks the download lifecycle from grab to import.

| | |
|---|---|
| Route | `POST /radarr` / `POST /sonarr` · Template `steps` (downloads) · Auth Basic |
| Slug | `radarr-movie-<tmdbId>` / `sonarr-series-<tvdbId>[-e-<episodeIds>]` (falls back to `<provider>-<downloadId>` when no TMDB/TVDB id is present, which lets retries of the same media collapse into one activity) |

Live Activity events (downloads):

| Event | State | Icon | Color |
|---|---|---|---|
| `Grab` | Grabbed | `arrow.down.circle` | blue |
| `ManualInteractionRequired` | Needs attention | `exclamationmark.triangle.fill` | orange |
| `Download` | Imported / Upgraded | `checkmark.circle.fill` | green |
| `Test` | provider-specific test activity | varies | varies |

Push notification events (no Live Activity, no template):

| Event | Notification |
|---|---|
| `Health` | Warning / Critical · (health message) |
| `HealthRestored` | Resolved · (health message) |

In the default `activity` mode, `Grab` and `Download` also write a notification record (not pushed) alongside the Live Activity; in `notify`/`smart` mode they are delivered as standalone push notifications instead.

Routing is governed by `starr.mode`: `activity` (default, all events → Live Activity), `notify` (all → push), or `smart` (handler decides per event).

**Setup:** In Radarr/Sonarr, go to **Settings > Connect > + > Webhook**. Set the URL to `https://relay.pushward.app/radarr` (or `/sonarr`). Leave Username as any value, set Password to your `hlk_` key (Basic Auth). Enable triggers: On Grab, On Import, On Health Issue, On Health Restored. Click Test, then Save.

### Prowlarr

Receives Prowlarr webhooks. All events are **push notifications**: indexer grabs are grouped into a thread derived from the parsed release base title (Prowlarr payloads carry no TMDB/TVDB id), and health and application-update events are sent as standalone pushes.

| | |
|---|---|
| Route | `POST /prowlarr` · Auth Basic |
| Thread | `prowlarr-<release-base-title>` (grabs) |

**Events:** `Grab` → push notification with indexer, size, and categories · `Health` / `HealthRestored` · `ApplicationUpdate` · `Test`.

> A Prowlarr `Test` event logs `unknown provider: prowlarr` instead of rendering a sample activity (Prowlarr has no self-test fixture). Its real `Grab` / `Health` events work normally.

**Setup:** In Prowlarr, go to **Settings > Connect > + > Webhook**. Set the URL to `https://relay.pushward.app/prowlarr`, leave Username as any value, set Password to your `hlk_` key (Basic Auth). Enable the triggers you want (On Grab, On Health Issue, etc.), then Test and Save.

### Bazarr

Receives Bazarr subtitle download notifications via Apprise. Sends a **push notification** (not a Live Activity) with the media title, language, and match score.

| | |
|---|---|
| Route | `POST /bazarr` · Auth Basic |
| CollapseID | `bazarr-<sha256(media)[:8]>` |

| Action | Title | Subtitle | Body |
|---|---|---|---|
| `downloaded` | Downloaded · language | media title | media title · Downloaded · language · score% |
| `upgraded` | Upgraded · language | media title | media title · Upgraded · language · score% |
| `manually downloaded` | Downloaded · language | media title | media title · Downloaded · language · score% |

**Setup:** In Bazarr, go to **Settings > Notifications**. Add a provider with this URL (the `hlk_` key is the Basic Auth password; the username can be anything):

```
jsons://user:hlk_YOUR_KEY@relay.pushward.app/bazarr
```

Enable subtitle download events, then Test and Save.

### Jellyfin

Receives Jellyfin webhook-plugin notifications. Tracks playback, library additions, scheduled tasks, and auth failures.

| | |
|---|---|
| Route | `POST /jellyfin` · Template `generic` (playback) · Auth Bearer |
| Slug (Live Activity) | `jellyfin-<hash>` (playback) |
| CollapseID (push) | `jellyfin-item-<itemId>`, `jellyfin-task-<taskName>`, `jellyfin-auth` |

Live Activity events (playback, `generic` template):

| Event | State | Icon | Color |
|---|---|---|---|
| `PlaybackStart` | Playing on (device) | `play.circle.fill` | blue |
| `PlaybackProgress` | Playing / Paused on (device) | `play.circle.fill` / `pause.circle.fill` | blue |
| `PlaybackStop` | Watched on (device) | `checkmark.circle.fill` | green |

Push notification events (no Live Activity, no template):

| Event | Notification |
|---|---|
| `ItemAdded` | Added · (media) |
| `ScheduledTaskStarted` | Started · (task) |
| `ScheduledTaskCompleted` | Complete · (task) / Failed · (task) |
| `AuthenticationFailure` | Failed login: (user) from (IP) |

`GenericUpdateNotification` triggers a provider-specific test activity.

**Debounce:** `PlaybackProgress` updates within `progress_debounce` (default `10s`) are skipped; play/pause state changes bypass the debounce. After `pause_timeout` (default `5m`) of being paused with no progress change, the activity auto-ends.

**Setup:** Install the [Webhook plugin](https://github.com/jellyfin/jellyfin-plugin-webhook). Go to **Dashboard > Plugins > Webhook**, add a Generic destination with URL `https://relay.pushward.app/jellyfin`. Under *Add Request Header*, set Key `Authorization` and Value `Bearer hlk_...`. Select notification types: Playback Start/Progress/Stop, Item Added, Task Started/Completed, Authentication Failure.

### Paperless-ngx

Receives document consumption webhooks. The JSON body is built from a Jinja2 template in the Paperless Workflows UI.

| | |
|---|---|
| Route | `POST /paperless` · Template `generic` · Auth Bearer |
| Slug | `paperless-<doc_id>` (added/updated), `paperless-<sha256(filename)[:8]>` (consumption_started) |

| Event | State | Icon | Color |
|---|---|---|---|
| `added` | Processed | `doc.text.fill` | green |
| `updated` | Updated | `doc.text.fill` | green |
| `consumption_started` | Processing… | `arrow.triangle.2.circlepath` | blue |

**Setup:** In Paperless-ngx, go to **Settings > Workflows** and create a workflow per event type. Action **Webhook**, URL `https://relay.pushward.app/paperless`, encoding JSON, body type Text, header `Authorization: Bearer hlk_...`.

Body template for **Document Added** (reuse for Updated with `"event":"updated"`):

```
{"event":"added","doc_id":{{doc_id}},"title":{{doc_title|tojson}},"correspondent":{{correspondent|tojson}},"document_type":{{document_type|tojson}},"doc_url":{{doc_url|tojson}},"filename":{{original_filename|tojson}}}
```

Body template for **Consumption Started** (only `original_filename` is available at this stage):

```
{"event":"consumption_started","filename":{{original_filename|tojson}}}
```

### Changedetection.io

Receives page-change notifications. The JSON body is a custom Jinja2 template in Changedetection's notification settings.

| | |
|---|---|
| Route | `POST /changedetection` · Template `alert` · Auth Bearer |
| Slug | `cd-<sha256(url)[:8]>` |

**Events:** single event — page changed. Creates a fire-and-forget alert (ONGOING + immediate ENDED). Icon `eye.fill`, color `#FF9500`, links `diff_url` and `preview_url`.

**Setup:** Set the notification URL to:

```
posts://relay.pushward.app/changedetection?+Authorization=Bearer+hlk_YOUR_KEY
```

Set `notification_format` to `custom` with this body:

```
{"url":{{watch_url|tojson}},"title":{{watch_title|tojson}},"tag":{{watch_tag|tojson}},"diff_url":{{diff_url|tojson}},"preview_url":{{preview_url|tojson}},"triggered_text":{{triggered_text|tojson}},"timestamp":{{notification_timestamp|tojson}}}
```

### Unmanic

Receives Apprise `json://` notifications from Unmanic on transcode completion or failure.

| | |
|---|---|
| Route | `POST /unmanic` · Template `generic` · Auth Bearer |
| Slug | `unmanic-<sha256(filename)[:8]>` |

| Type | State | Icon | Color |
|---|---|---|---|
| `success` | Complete | `checkmark.circle.fill` | green |
| `failure` | Failed | `xmark.circle.fill` | red |
| `info` | provider-specific test activity | varies | varies |

**Setup:** In Unmanic, go to **Settings > Notifications** and add:

```
jsons://relay.pushward.app/unmanic?+Authorization=Bearer+hlk_YOUR_KEY
```

### Proxmox VE

Receives Proxmox VE notification webhooks for backup, replication, fencing, package-update, and general system (`system-mail`) events. The Datacenter test button is supported too.

| | |
|---|---|
| Route | `POST /proxmox` · Template `steps` (backup/replication), `alert` (fencing/package-updates/system-mail) · Auth Bearer |
| Slug | `proxmox-backup-<hash>`, `proxmox-repl-<hash>`, `proxmox-fence-<hash>`, `proxmox-updates-<hash>`, `proxmox-system-<hash>` |

| Event | State | Icon | Color |
|---|---|---|---|
| `vzdump` (start) | Backing up… | `externaldrive.fill.badge.timemachine` | blue |
| `vzdump` (complete) | Backup Complete | `checkmark.circle.fill` | green |
| `vzdump` (failed) | Backup Failed | `xmark.circle.fill` | red |
| `replication` (start) | Replicating… | `arrow.triangle.2.circlepath` | blue |
| `replication` (complete) | Replication Complete | `checkmark.circle.fill` | green |
| `replication` (failed) | Replication Failed | `xmark.circle.fill` | red |
| `fencing` | (title) | `exclamationmark.octagon.fill` | red |
| `package-updates` | (title) | `arrow.down.circle` | blue |
| `system-mail` | (title) | `bell.fill` / `exclamationmark.triangle.fill` | by severity (blue/orange/red) |
| test button (empty `type`) | test notification | varies | varies |

**Setup:** In Proxmox VE, go to **Datacenter > Notifications** and add a webhook target:

- **URL:** `https://relay.pushward.app/proxmox`
- **Method:** POST
- **Headers:** `Content-Type: application/json` and `Authorization: Bearer {{ secrets.token }}`
- **Secrets:** add key `token` with your `hlk_` integration key
- **Body:**

```
{"type":"{{ fields.type }}","title":"{{ escape title }}","message":"{{ escape message }}","severity":"{{ severity }}","hostname":"{{ fields.hostname }}"}
```

A target on its own does nothing: Proxmox only calls it when a **matcher** selects a notification and lists that target. Add `PushWard` as a target on a matcher, either the built-in `default-matcher` or a dedicated one. In the UI go to **Datacenter > Notifications**, edit a matcher, and add `PushWard` under the targets to notify (a matcher can list several, so mail and PushWard both fire). Or from the shell:

```
pvesh set /cluster/notifications/matchers/default-matcher --target mail-to-root --target PushWard
```

`--target` replaces the whole list, so pass the existing targets too or you'll drop them. Skip this step and the webhook is never called: the target exists but no event reaches it.

The test button on the same screen sends a webhook with an empty `type`, which the relay handles as a self-test so you can confirm delivery without waiting for a real event.

### Overseerr / Jellyseerr

Receives media request webhooks. Tracks the request lifecycle from pending to available.

| | |
|---|---|
| Route | `POST /overseerr` · Template `steps` · Auth Bearer |
| Slug | `overseerr-<mediaType>-<tmdbId>` |

| Event | State | Step | Color |
|---|---|---|---|
| `MEDIA_PENDING` | Requested | 1/4 | orange |
| `MEDIA_APPROVED` / `MEDIA_AUTO_APPROVED` | Approved | 2/4 | blue |
| `MEDIA_AVAILABLE` | Available | 4/4 | green |
| `MEDIA_DECLINED` | Declined | – | red |
| `MEDIA_FAILED` | Failed | – | red |
| `TEST_NOTIFICATION` | test notification | – | varies |

**Setup:** In Overseerr/Jellyseerr, go to **Settings > Notifications > Webhook**. Set the Webhook URL to `https://relay.pushward.app/overseerr`, the Authorization Header to `Bearer hlk_...`, and the JSON Payload to:

```json
{
  "notification_type": "{{notification_type}}",
  "subject": "{{subject}}",
  "message": "{{message}}",
  "image": "{{image}}",
  "{{media}}": {},
  "{{request}}": {},
  "{{extra}}": []
}
```

Enable: Request Pending, Approved, Available, Declined, Failed.

### Uptime Kuma

Receives monitor status webhooks. Maps monitor heartbeat status to alert notifications.

| | |
|---|---|
| Route | `POST /uptimekuma` · Template `alert` · Auth Bearer |
| Slug | `uptime-<monitorId>` |

| Status | State | Icon | Color |
|---|---|---|---|
| `0` (DOWN) | (heartbeat message or "Monitor Down") | `exclamationmark.triangle.fill` | red |
| `1` (UP) | Resolved | `checkmark.circle.fill` | green |
| `2` (PENDING) | Checking… | `hourglass` | orange |
| `3` (MAINTENANCE) | test notification | varies | varies |

**Setup:** In Uptime Kuma, go to **Settings > Notifications > Setup Notification**. Type **Webhook**, Post URL `https://relay.pushward.app/uptimekuma`, Request Body JSON. In *Additional Headers*: `{"Authorization": "Bearer hlk_..."}`. Check *Default Enabled* to apply to all monitors.

### Gatus

Receives health-check alert webhooks. Maps endpoint TRIGGERED/RESOLVED states to alert notifications.

| | |
|---|---|
| Route | `POST /gatus` · Template `alert` · Auth Bearer |
| Slug | `gatus-<sha256(group/endpoint_name)[:12]>` |

| Status | State | Icon | Color |
|---|---|---|---|
| `TRIGGERED` | (error details) | `exclamationmark.triangle.fill` | red |
| `RESOLVED` | Resolved | `checkmark.circle.fill` | green |

**Setup:** In your `gatus.yaml`, configure `alerting.custom`:

```yaml
alerting:
  custom:
    url: "https://relay.pushward.app/gatus"
    method: "POST"
    headers:
      Content-Type: "application/json"
      Authorization: "Bearer hlk_..."
    body: |
      {
        "endpoint_name": "[ENDPOINT_NAME]",
        "endpoint_group": "[ENDPOINT_GROUP]",
        "endpoint_url": "[ENDPOINT_URL]",
        "alert_description": "[ALERT_DESCRIPTION]",
        "status": "[ALERT_TRIGGERED_OR_RESOLVED]",
        "result_errors": "[RESULT_ERRORS]"
      }
```

Reference `type: custom` in your endpoint alerts with `send-on-resolved: true`.

### Backrest

Receives backup operation webhooks for snapshot, prune, check, and forget operations.

| | |
|---|---|
| Route | `POST /backrest` · Template `steps` (operations), `alert` (errors/skipped) · Auth Bearer |
| Slug | `backrest-<sha256(plan+repo)[:8]>` |

| Condition | State | Icon | Color |
|---|---|---|---|
| `CONDITION_SNAPSHOT_START` | Backing up… | `arrow.triangle.2.circlepath` | blue |
| `CONDITION_SNAPSHOT_SUCCESS` | Complete (+ data added) | `checkmark.circle.fill` | green |
| `CONDITION_SNAPSHOT_WARNING` | Complete (warnings) | `exclamationmark.triangle.fill` | orange |
| `CONDITION_SNAPSHOT_ERROR` | Failed | `xmark.circle.fill` | red |
| `CONDITION_PRUNE_START` | Pruning… | `arrow.triangle.2.circlepath` | blue |
| `CONDITION_PRUNE_SUCCESS` | Pruned | `checkmark.circle.fill` | green |
| `CONDITION_PRUNE_ERROR` | Prune Failed | `xmark.circle.fill` | red |
| `CONDITION_CHECK_START` | Checking… | `arrow.triangle.2.circlepath` | blue |
| `CONDITION_CHECK_SUCCESS` | Check Passed | `checkmark.circle.fill` | green |
| `CONDITION_CHECK_ERROR` | Check Failed | `xmark.circle.fill` | red |
| `CONDITION_FORGET_START` | Applying retention… | `arrow.triangle.2.circlepath` | blue |
| `CONDITION_FORGET_SUCCESS` | Retention applied | `checkmark.circle.fill` | green |
| `CONDITION_FORGET_ERROR` | Retention failed | `xmark.circle.fill` | red |
| `CONDITION_ANY_ERROR` | (error message) | `exclamationmark.triangle.fill` | red |
| `CONDITION_SNAPSHOT_SKIPPED` | Snapshot Skipped | `info.circle.fill` | blue |

**Setup:** In Backrest, on the Plan or Repo, under *Hooks* click **+ Add Hook** and select **Shoutrrr**. Select the 15 conditions above and set *On Error* to "Ignore". Set the **Shoutrrr URL** (the `@authorization` param adds the header):

```
generic+https://relay.pushward.app/backrest?@authorization=Bearer+hlk_YOUR_KEY&contenttype=application/json
```

Set the **Template** (Go template that renders the JSON body):

```
{"event":"{{ .Event }}","plan":"{{ .Plan.Id }}","repo":"{{ .Repo.Id }}","snapshot_id":"{{ .SnapshotId }}","data_added":{{ if .SnapshotStats }}{{ .SnapshotStats.DataAdded }}{{ else }}0{{ end }},"error":"{{ .Error }}"}
```

### Gitea

Receives Gitea Actions webhooks and renders a run as a live build-progress Live Activity. Jobs are grouped into steps (matrix jobs fold into one group), and the activity is reused across consecutive runs of the same repo.

| | |
|---|---|
| Route | `POST /gitea` · Template `steps` · Auth Bearer |
| Slug | `gitea-<sha256(repo_full_name)[:8]>` (one activity per repo) |

| Event | Behavior |
|---|---|
| `workflow_run` requested / in_progress | Creates the activity, seeds "Queued"/"Running" |
| `workflow_job` queued / in_progress / completed | Updates per-job step progress |
| `workflow_run` completed | Final frame (Success/Failed/Cancelled/Skipped) then two-phase end |

A newer run supersedes an older one on the same repo; events for an older run, or jobs arriving after a run completed, are dropped. Runs with more than 10 step groups drop the per-step labels to stay inside the APNs payload budget.

**Setup:** In Gitea, go to the repo (or org) **Settings > Webhooks > Add Webhook > Gitea**. Set the URL to `https://relay.pushward.app/gitea`, set **Authorization Header** to `Bearer hlk_...`, and under **Custom Events** enable **Workflow Run** and **Workflow Job**. Requires Gitea 1.24+ for `workflow_job` and 1.25+ for `workflow_run`; 1.26+ is recommended (earlier versions do not emit run-level `in_progress`).

### Forgejo

Receives Forgejo Actions webhooks. Forgejo emits only terminal run events (`action_run_success` / `action_run_failure` / `action_run_recover`), so it shows a completion result rather than live per-job progress.

| | |
|---|---|
| Route | `POST /forgejo` · Template `generic` · Auth Bearer |
| Slug | `forgejo-<sha256(repo_full_name)[:8]>` (one activity per repo) |

| Action | State | Icon | Color |
|---|---|---|---|
| `success` | Succeeded | `checkmark.circle.fill` | green |
| `recover` | Recovered | `checkmark.circle.fill` | green |
| `failure` | Failed | `xmark.circle.fill` | red |

**Setup:** In Forgejo, add a webhook the same way (repo/org **Settings > Webhooks**), URL `https://relay.pushward.app/forgejo`, **Authorization Header** `Bearer hlk_...`, and enable the **Action Run** events. If a future Forgejo release adds `workflow_run`/`workflow_job` webhooks, point it at `/gitea` instead for live progress. The exact minimum Forgejo version shipping the `action_run_*` events is not pinned here; check your Forgejo release notes.

### Komodo

Receives Komodo Custom-alerter events. Resolvable server conditions become a Live Activity that resolves when Komodo clears them; every other alert is a one-shot push notification.

| | |
|---|---|
| Route | `POST /komodo` · Auth Basic (via URL userinfo) |
| Slug | `komodo-<sha256(target_type/target_id/data_type)[:12]>` |

| Alert kind | Output |
|---|---|
| Resolvable (`ServerUnreachable`, `ServerCpu`, `ServerMem`, `ServerDisk`, `ServerVersionMismatch`, `SwarmUnhealthy`) | Live Activity (alert) that resolves on clear + active/passive push |
| One-shot (container/stack state change, image update, build/procedure/action failed, sync pending, scheduled run, custom, ...) | Push notification (OK -> passive, WARNING -> active, CRITICAL -> time-sensitive) |
| `Test` | Test Live Activity |

The activity is keyed on the alert condition (target + type), not the alert id, so a resolve collapses onto the same activity as its trigger. The resolve frame always renders "Resolved" (the payload's carried error is stale by then).

**Setup:** In Komodo, go to **Settings > Alerters** and add a **Custom** alerter. Store your `hlk_` key as a Komodo Secret and set the alerter URL with the key in userinfo: `https://pushward:[[PUSHWARD_KEY]]@relay.pushward.app/komodo`. Komodo posts via reqwest, which turns the URL userinfo into an HTTP Basic `Authorization` header that the relay reads the key from.

### TrueNAS

Emulates the OpsGenie alert service that TrueNAS ships with. TrueNAS opens an alert with `POST /v2/alerts` and clears it with `DELETE /v2/alerts/{alias}`, so each alert becomes a Live Activity that ends when TrueNAS clears it.

| | |
|---|---|
| Route | `POST /truenas/v2/alerts` · `DELETE /truenas/v2/alerts/{id}` · Auth GenieKey |
| Slug | `truenas-<sha256(alias)[:12]>` |

| Call | Behavior |
|---|---|
| `POST /v2/alerts` | Creates the activity (alert, warning/orange) + active push |
| `DELETE /v2/alerts/{alias}` | Ends the activity (Resolved, green) + passive push; unknown alias is a no-op |

**Setup:** In TrueNAS, go to **System Settings > Alert Services > Add**. Set **Type** to **OpsGenie**, **API Key** to your `hlk_` key, and **API URL** to `https://relay.pushward.app/truenas` (no trailing slash). Pick the alert **Level** to forward, then **Send Test Alert** to verify (a test flows as a real create then clear).

**Limitations:** TrueNAS's OpsGenie payload carries no hostname (multi-NAS setups cannot tell boxes apart in the activity) and no severity level, so alerts render with a fixed warning style; filter what you forward using the per-service **Level** in TrueNAS. The API URL must have no trailing slash.

## Development

Commands match CI (`go-cicd-reusable.yml`, which builds with `go_module_path: ./relay`, `go_test_args: -race -count=1 -v`).

```bash
# Build (workspace root)
go build ./relay/cmd/pushward-relay

# All relay tests
go test ./relay/... -v -count=1

# With the race detector (matches CI)
go test ./relay/... -race -count=1 -v

# Single provider
go test ./relay/internal/grafana/... -run TestGrafana -v -count=1

# Lint (matches CI)
golangci-lint run

# Docker (context is the repo root so the Dockerfile can COPY shared/)
docker build -f relay/Dockerfile -t pushward-relay .
docker build -f relay/Dockerfile --build-arg GO_VERSION=1.26.5 -t pushward-relay .
```

> DB state tests (`relay/internal/state/...`) use testcontainers-go and require a running Docker daemon.

## CI/CD & Releases

Bridges are versioned **independently**. Tag format: `<bridge>/v<X.Y.Z>` (e.g. `relay/v0.4.1`). Pushing the tag triggers `release.yml`, which builds and publishes images with auto-generated changelog notes (categorized via `.github/release.yml`).

Images publish to **GHCR** (`ghcr.io/mac-lucky/pushward-relay`). The image-tag channels:

| Trigger | Tags published | Purpose |
|---|---|---|
| Pull request | _(none)_ | Tests + analysis only |
| Push to `main` | `:main`, `:main-<short-sha>` | Rolling latest + immutable per-commit pin |
| Git tag `relay/v<X.Y.Z>` | `:X.Y.Z`, `:X.Y`, `:latest` (and `:X` once `X >= 1`) | Stable release |

`:latest` moves only on a tagged release — never on a `main` push.

```bash
# Single-bridge release
git tag relay/v0.4.1
git push origin relay/v0.4.1
```

## Server compatibility

The relay calls the [pushward-server](https://pushward.app) REST API to create/update/end **activities** (`POST /activities`, `PATCH /activities/{slug}`) and to send notifications — the server then delivers via APNs to the iOS app. The API contract (endpoints, JSON shape, auth headers) is owned by pushward-server's `openapi.yaml`. The relay targets that surface at its `MAJOR.MINOR`; patch releases (`relay/v*.*.X`) are bridge-only fixes that need no coordinated server bump.

## Troubleshooting

Logs are structured JSON on stdout (`slog`). View them with `docker logs <container>` or your platform's log viewer; each request log includes a hashed `tenant` field for correlation (the raw `hlk_` key is never logged).

| Symptom | Likely cause | Fix |
|---|---|---|
| `401` on every webhook | No valid `hlk_` key in the `Authorization` header. | Send `Bearer hlk_...`, or for Radarr/Sonarr/Prowlarr/Bazarr put the key in the Basic Auth **password**. |
| `429` responses | Per-IP (5 r/s) or per-key (1 r/s) rate limit. | Slow the sender, or set `trusted_proxy_cidrs` so per-IP limiting uses the real client IP instead of the proxy IP. |
| All traffic shares one IP bucket | Running behind a reverse proxy/Cloudflare without `trusted_proxy_cidrs`. A startup warning is logged. | Set `PUSHWARD_TRUSTED_PROXY_CIDRS` to your proxy's CIDRs. |
| `config load` fails: `metrics_address must differ from address` | `server.metrics_address` equals `server.address`. | Use different ports (defaults `:8090` / `:9090`), or set `metrics_address` empty to disable metrics. |
| `config load` fails: `stale_timeout must be > 0` | A provider has a non-positive `stale_timeout`. | Set a positive duration (a non-positive TTL writes state rows that are never cleaned up). |
| `/ready` returns `503` | DB ping check failed. | Verify `PUSHWARD_DATABASE_DSN` / `password_file` and that PostgreSQL is reachable. |
| Prowlarr `Test` logs `unknown provider: prowlarr` | Prowlarr has no self-test fixture. | Expected — real `Grab`/`Health` events still work. |
| Upstream `401`/`403`/`429` surfaced to the sender | The PushWard server rejected the `hlk_` key or rate-limited. | Check the key is valid and has capacity; the relay forwards these statuses so the source app reports the real cause. |

## Requirements & License

- **Go** `1.26.x` (toolchain `1.26.5`; Docker builds default to `golang:1.26.5-alpine`, final image `alpine:3.23`).
- **PostgreSQL** for the state store.
- A running **PushWard server** and a per-tenant `hlk_` integration key.

Part of the public [`pushward-integrations`](https://github.com/mac-lucky/pushward-integrations) repository — see the repository root for license terms.
