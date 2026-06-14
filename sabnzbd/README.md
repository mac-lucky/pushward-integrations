[![Website](https://img.shields.io/badge/pushward.app-5B4FE5?style=for-the-badge&logo=safari&logoColor=white)](https://pushward.app)
[![App Store](https://img.shields.io/badge/App_Store-Download-0D96F6?style=for-the-badge&logo=apple&logoColor=white)](https://apps.apple.com/app/id6759689999)
[![CI/CD SABnzbd](https://github.com/mac-lucky/pushward-integrations/actions/workflows/sabnzbd-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/sabnzbd-ci-cd.yml)
[![Image](https://img.shields.io/badge/ghcr.io-pushward--sabnzbd-2496ED?logo=docker&logoColor=white)](https://github.com/mac-lucky/pushward-integrations/pkgs/container/pushward-sabnzbd)

# PushWard for SABnzbd

Tracks SABnzbd downloads and post-processing as a live-updating [PushWard](https://pushward.app) Live Activity on iPhone (Dynamic Island + Lock Screen). SABnzbd calls the bridge's webhook when an NZB is added; the bridge then polls the SABnzbd JSON API through the download and unpack phases and pushes progress, speed, ETA, and a completion summary to the PushWard server.

> **New to PushWard?** Learn what it does at **[pushward.app](https://pushward.app)** and get the iOS app on the **[App Store](https://apps.apple.com/app/id6759689999)**.

## How it works

```
SABnzbd (NZB added)  ──webhook──▶  pushward-sabnzbd  ──REST──▶  PushWard server  ──APNs──▶  iOS Live Activity
                         polls SABnzbd JSON API (queue + history)
```

`pushward-sabnzbd` is a **standalone (single-tenant) bridge**: it uses one PushWard integration key (`hlk_…`) for every push. On a webhook it creates one activity (slug `sabnzbd`, name "SABnzbd"), then polls SABnzbd on an interval and merge-patches the activity as progress changes. On startup it also resumes tracking if a download or unpack is already in flight, and dismisses any stale activity left over from a previous crash.

## Features

- **Multi-file tracking** — shows the current filename and an `X/Y` counter when multiple NZBs are queued.
- **Live download readout** — progress, speed (MB/s), and an ETA countdown derived from SABnzbd's `timeleft`.
- **Post-processing phases** — Verifying, Repairing, Extracting, and Moving each render with a distinct SF Symbol icon.
- **Completion summary** — total size, average speed, and unpack time, e.g. `1.2 GB · 45 MB/s avg · unpack 2m 3s`.
- **Two templates** — `generic` (default) or `timeline`, which renders a download-speed sparkline.
- **Paused state** — reflects SABnzbd pause/resume on the activity.
- **Resume on startup** — picks up an in-progress download or unpack after a restart; otherwise clears a stale activity.
- **Two-phase end** — sends a final ONGOING frame (so the APNs push-update token delivers it), then ENDED to dismiss.
- **Change-detection heartbeat** — skips redundant pushes but pings at least every 30s so the server's stale TTL doesn't auto-end the activity.
- **Resilient client** — circuit breaker (5 failures / 30s cooldown) plus 5x retry with exponential backoff on 5xx/network errors and `Retry-After` on 429.
- **Webhook secret** — optional constant-time `X-Webhook-Secret` validation.
- **Hardened by default** — non-root container (UID 1000), 1 MiB request body cap, and the SABnzbd API key redacted from logs.

## Prerequisites

- A running **PushWard server** (public API: `https://api.pushward.app`).
- A **PushWard integration key** (`hlk_` prefix).
- A **SABnzbd** instance reachable over HTTP with its API key.
- The **PushWard iOS app** installed and subscribed to the `sabnzbd` activity slug.

## Installation

The published image is multi-arch (`linux/amd64`, `linux/arm64`).

### Docker

```bash
docker run -p 8090:8090 \
  -e PUSHWARD_SABNZBD_URL="https://sabnzbd.example.com/api" \
  -e PUSHWARD_SABNZBD_API_KEY="YOUR_SABNZBD_API_KEY" \
  -e PUSHWARD_URL="https://api.pushward.app" \
  -e PUSHWARD_API_KEY="hlk_xxxxxxxxxxxx" \
  ghcr.io/mac-lucky/pushward-sabnzbd:latest
```

The image entrypoint defaults to `-config /config/config.yml`. To use a file instead of env vars, mount one:

```bash
docker run -p 8090:8090 -v ./config.yml:/config/config.yml:ro ghcr.io/mac-lucky/pushward-sabnzbd:latest
```

### Docker Compose

```yaml
services:
  pushward-sabnzbd:
    image: ghcr.io/mac-lucky/pushward-sabnzbd:latest
    ports:
      - "8090:8090"
    environment:
      - PUSHWARD_SABNZBD_URL=https://sabnzbd.example.com/api
      - PUSHWARD_SABNZBD_API_KEY=YOUR_SABNZBD_API_KEY
      - PUSHWARD_URL=https://api.pushward.app
      - PUSHWARD_API_KEY=hlk_xxxxxxxxxxxx
      # Optional but recommended when the webhook is reachable beyond localhost:
      - PUSHWARD_SABNZBD_WEBHOOK_SECRET=YOUR_WEBHOOK_SECRET
      # Optional: speed sparkline instead of the generic template:
      # - PUSHWARD_SABNZBD_TEMPLATE=timeline
```

Images are published to GHCR only: `ghcr.io/mac-lucky/pushward-sabnzbd`.

### Point SABnzbd at the webhook

In SABnzbd, go to **Config → Notifications → Notification Script / URL** and POST to the bridge on the **NZB added** event:

```
http://<pushward-sabnzbd-host>:8090/webhook
```

If you set `PUSHWARD_SABNZBD_WEBHOOK_SECRET`, send the same value in an `X-Webhook-Secret` request header.

## Configuration

Settings come from a YAML file (`-config`) and/or environment variables. **Env vars override YAML.** The standardized env prefix is `PUSHWARD_*`. See [`config.example.yml`](./config.example.yml) for the canonical, commented file.

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_SABNZBD_URL` | `sabnzbd.url` | SABnzbd JSON API base URL (e.g. `http://host:8080/api`). The client appends `apikey`/`output`/`mode` query params. | **Yes** |
| `PUSHWARD_SABNZBD_API_KEY` | `sabnzbd.api_key` | SABnzbd API key, sent as the `apikey` query parameter (SABnzbd 4.5+ has no header auth). Redacted from logs. | **Yes** |
| `PUSHWARD_URL` | `pushward.url` | PushWard server base URL (public: `https://api.pushward.app`). | **Yes** |
| `PUSHWARD_API_KEY` | `pushward.api_key` | PushWard integration key (`hlk_` prefix); the single tenant key for all pushes. | **Yes** |
| `PUSHWARD_SABNZBD_WEBHOOK_SECRET` | `sabnzbd.webhook_secret` | Shared secret; when set, `/webhook` requires a matching `X-Webhook-Secret` header. Empty = unauthenticated (logged as a warning). | No (empty) |
| `PUSHWARD_SABNZBD_TEMPLATE` | `sabnzbd.template` | Live Activity template: `generic` (default) or `timeline`. | No (`generic`) |
| `PUSHWARD_SERVER_ADDRESS` | `server.address` | HTTP listen address for `/webhook`, `/health`, `/ready`. | No (`:8090`) |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Activity priority, validated 0–10. | No (`1`) |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Server-side stale TTL (`staleTTL`) for the activity; 30s heartbeats keep it from auto-ending mid-download. | No (`30m`) |
| `PUSHWARD_END_DELAY` | `pushward.end_delay` | Delay before phase 1 of the two-phase end (ONGOING with final content). | No (`5s`) |
| `PUSHWARD_END_DISPLAY_TIME` | `pushward.end_display_time` | How long the completion frame shows before phase 2 (ENDED dismiss). | No (`4s`) |
| `PUSHWARD_POLL_INTERVAL` | `polling.interval` | SABnzbd poll interval during tracking; validated to be at least `1s`. | No (`5s`) |

### Timeline display (template: `timeline` only)

These keys are **YAML-only** (no env override) and apply only when `sabnzbd.template` is `timeline`. The sparkline plots the `Speed` series in MB/s.

```yaml
sabnzbd:
  template: "timeline"
  timeline:
    smoothing: true     # smooth sparkline curve interpolation
    scale: "linear"     # "linear" or "logarithmic"
    decimals: 0         # value-label precision, 0-10
```

| Config Key | Description | Default |
|---|---|---|
| `sabnzbd.timeline.smoothing` | Smooth the sparkline curve. | `true` |
| `sabnzbd.timeline.scale` | Y-axis scale: `linear` or `logarithmic`. | `linear` |
| `sabnzbd.timeline.decimals` | Value-label precision (0–10). | `0` |

> **Accepted but unused by this bridge:** `pushward.cleanup_delay` (default `15m`) is read and logged at startup but never schedules a cleanup — leave it at the default. `server.metrics_address` exists in the shared config but this bridge starts no metrics server, so there is nothing to scrape.

### Security note on `sabnzbd.url`

SABnzbd accepts the API key only as a URL query parameter, so over plain `http://` an intermediate proxy or router could capture it from access logs. The bridge redacts the key from its own logs; if SABnzbd is exposed beyond a trusted network, front it with HTTPS.

## Endpoints

| Method | Path | Description |
|---|---|---|
| `POST` | `/webhook` | Called by SABnzbd on NZB-added; starts/queues tracking. `200 {"status":"tracking_started"}`, or `{"status":"already_tracking"}` if a track is running. `405` on non-POST; `401` if a secret is configured and `X-Webhook-Secret` is missing/wrong. Body capped at 1 MiB. |
| `GET` | `/health` | Liveness — always `200 ok`. |
| `GET` | `/ready` | Readiness — `200 ready` (no readiness checks registered, so effectively always ready). |

## What the activity looks like

The bridge maps one SABnzbd session to a single Live Activity (slug `sabnzbd`):

1. **Seed** — creates the activity (`endedTTL=0` so the slug persists for reuse, `staleTTL` = `stale_timeout`) and shows `Starting…`.
2. **Wait for start** — polls the queue a bounded number of times (12 polls ≈ 60s at the default 5s interval; scales with `polling.interval`). If the queue stays idle and nothing is post-processing, it ends with `No downloads`.
3. **Download** — current filename + `X/Y` counter, progress, speed (MB/s), ETA, and a paused state.
4. **Post-processing** — Verifying / Repairing / Extracting / Moving with phase-specific icons.
5. **Continue** — if more downloads appear in the queue, loops back to the download phase.
6. **Complete** — green checkmark with a summary like `1.2 GB · 45 MB/s avg · unpack 2m 3s`; subtitle is the last completed name.
7. **End** — two-phase: a final ONGOING frame (so the push-update token delivers the summary), then ENDED to dismiss. Resumed sessions skip the dance and send ENDED directly.

## Development

This bridge is one module in the `pushward-integrations` Go workspace (`go.work`); run these from the **repo root**.

```bash
# Build the binary
go build ./sabnzbd/cmd/pushward-sabnzbd

# Run with a config file
./pushward-sabnzbd -config sabnzbd/config.example.yml

# Test this bridge plus the shared module (matches CI)
go test ./shared/... ./sabnzbd/... -race -count=1 -v

# Lint (matches CI)
golangci-lint run
```

Docker builds use the **repo root as the build context** with `-f sabnzbd/Dockerfile` so the Dockerfile can `COPY shared/`:

```bash
# Build the image (note the trailing "." = repo root, not the sabnzbd dir)
docker build -f sabnzbd/Dockerfile -t pushward-sabnzbd .

# Optional: override the Go toolchain (Dockerfile default ARG GO_VERSION=1.26.4, matching go.mod)
docker build -f sabnzbd/Dockerfile --build-arg GO_VERSION=1.26.4 -t pushward-sabnzbd .
```

## CI/CD & Releases

Bridges in this repo are versioned independently. Tag format is `sabnzbd/v<X.Y.Z>` (e.g. `sabnzbd/v1.0.0`); pushing the tag runs the release pipeline and publishes the version images. A changelog is auto-generated via `.github/release.yml`.

| Trigger | Tags published |
|---|---|
| Pull request | none (build/test only) |
| Push to `main` | `:main`, `:main-<sha>` |
| Tag `sabnzbd/v<X.Y.Z>` | `:X.Y.Z`, `:X.Y`, `:latest` (and `:X` once X ≥ 1) |

`:latest` only moves on a tagged release.

## Server compatibility

This bridge targets the PushWard server REST API and uses `POST /activities`, `PATCH /activities/{slug}` (full and merge-patch), and the APNs Live Activity `ContentState` shape. The contract owner is **pushward-server** (`openapi.yaml`); the shared `pushward.Client` is hand-written and kept in sync with it. Bridges track the server API surface at its **MAJOR.MINOR** — a patch release is a bridge-only fix. The released iOS clients cannot be hot-fixed, so the `sabnzbd` slug, content keys, and casing must stay stable.

## Troubleshooting

Logs are structured JSON on stdout (`slog`, Info level). Read them with:

```bash
docker logs -f pushward-sabnzbd
```

| Symptom | Cause / fix |
|---|---|
| `sabnzbd.url is required` / `sabnzbd.api_key is required` | Set `PUSHWARD_SABNZBD_URL` / `PUSHWARD_SABNZBD_API_KEY` (or the YAML keys). |
| `polling.interval must be at least 1s` | Raise `PUSHWARD_POLL_INTERVAL` to `1s` or more. |
| `webhook secret not configured — webhook endpoint is unauthenticated` | Expected when no secret is set; set `PUSHWARD_SABNZBD_WEBHOOK_SECRET` to require `X-Webhook-Secret`. |
| Webhook returns `401 unauthorized` | A secret is configured but the request's `X-Webhook-Secret` is missing or wrong. |
| `SABnzbd never started downloading, giving up` | The queue stayed idle after the bounded wait and nothing was post-processing — check that SABnzbd actually queued the NZB and isn't paused. |
| No Live Activity on the phone | Confirm the iOS app is subscribed to the `sabnzbd` slug, the `hlk_` key is valid, and `PUSHWARD_URL` points at your server. |
| `fetching queue` / `unexpected status` errors | The bridge can't reach SABnzbd (5s HTTP timeout) or the API key is wrong — verify `sabnzbd.url` resolves and the key is correct (it is redacted in logs). |

## Requirements & License

- Go 1.26.x (build), Docker (deploy).
- A running PushWard server, a SABnzbd instance, and the PushWard iOS app.

Part of the public [pushward-integrations](https://github.com/mac-lucky/pushward-integrations) repository — see the repository root for license details.
