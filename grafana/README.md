# pushward-grafana

Bridges Grafana alert notifications to [PushWard](https://github.com/mac-lucky/pushward-server) Live Activities on iOS.

Exposes a webhook endpoint that Grafana calls via its contact point configuration. Firing alerts appear as Live Activities on your iPhone's Dynamic Island and Lock Screen, color-coded by severity. Resolved alerts automatically dismiss.

## Features

- **Alert grouping** — multiple instances of the same alert rule (by `alertname`) are grouped into a single Live Activity, with the worst severity driving the icon and color
- **Severity-based styling** — critical (red), warning (orange), and info (blue) alerts each get a distinct icon and accent color
- **URL links** — each activity links to the alert rule in Grafana (`generatorURL`) and the associated panel or dashboard (`panelURL`/`dashboardURL`)
- **Fired-at timestamp** — the `startsAt` time is sent to the iOS app for display on the Live Activity
- **Auto-resolved** — resolved alerts show a green checkmark and the server auto-cleans up after `ended_ttl` expires
- **Stale detection** — alerts with no updates for 24h are auto-ended via server-side `stale_ttl`
- **Alertname-based slugs** — each alert rule gets its own Live Activity (`grafana-<sha256(alertname)[:6]>`)
- **Webhook secret** — optional `X-Webhook-Secret` header validation for securing the endpoint
- **Auto-activity management** — creates the PushWard activity on first webhook, no manual setup needed
- **Retry with backoff** — PushWard API calls retry up to 5 times with exponential backoff
- **Graceful shutdown** — waits for in-flight requests on SIGINT/SIGTERM

## Prerequisites

- A running PushWard server
- A PushWard integration key (`hlk_` prefix)
- A Grafana instance with alerting configured
- The PushWard iOS app subscribed to the activity

## Configuration

All settings can be provided via YAML config file or environment variables. Environment variables take precedence.

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_URL` | `pushward.url` | PushWard server URL | Yes |
| `PUSHWARD_API_KEY` | `pushward.api_key` | PushWard integration key (`hlk_`) | Yes |
| `PUSHWARD_GRAFANA_SEVERITY_LABEL` | `grafana.severity_label` | Label key to read severity from (default: `severity`) | No |
| `PUSHWARD_GRAFANA_DEFAULT_SEVERITY` | `grafana.default_severity` | Fallback severity when label missing (default: `warning`) | No |
| `PUSHWARD_GRAFANA_DEFAULT_ICON` | `grafana.default_icon` | Default SF Symbol icon (default: `exclamationmark.triangle.fill`) | No |
| `PUSHWARD_GRAFANA_WEBHOOK_SECRET` | `grafana.webhook_secret` | Optional secret for `X-Webhook-Secret` header validation | No |
| `PUSHWARD_SERVER_ADDRESS` | `server.address` | HTTP listen address (default: `:8090`) | No |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Activity priority 0-10 (default: `5`) | No |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | Passed as `ended_ttl` to the server — how long the server keeps an ended activity before deletion (default: `5m`) | No |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Passed as `stale_ttl` to the server — auto-ends alerts with no updates after this duration (default: `24h`) | No |

## Docker Compose

```yaml
services:
  pushward-grafana:
    image: ghcr.io/mac-lucky/pushward-grafana:latest
    ports:
      - "8090:8090"
    volumes:
      - ./config.yml:/config/config.yml:ro
    environment:
      - PUSHWARD_URL=https://api.pushward.app
      - PUSHWARD_API_KEY=hlk_xxxxxxxxxxxx
```

## Grafana Webhook Setup

In Grafana, go to **Alerting > Contact points** and create a new contact point:

1. Set **Integration** to **Webhook**
2. Set the **URL** to:
   ```
   http://<pushward-grafana-host>:8090/webhook
   ```
3. Save and assign it to a notification policy

Alerts should include a `severity` label (or configure `grafana.severity_label` to match your label key).

## Endpoints

| Method | Path | Description |
|---|---|---|
| POST | `/webhook` | Receives Grafana alert webhooks |
| GET | `/health` | Health check (returns `ok`) |

## Severity Mapping

| Severity | Icon | Color |
|---|---|---|
| `critical` | `exclamationmark.octagon.fill` | `#FF3B30` (red) |
| `warning` | `exclamationmark.triangle.fill` | `#FF9500` (orange) |
| `info` | `info.circle.fill` | `#007AFF` (blue) |
| Resolved | `checkmark.circle.fill` | `#34C759` (green) |

## How It Works

1. **Webhook** — Grafana sends a POST to `/webhook` when alerts fire or resolve
2. **Grouping** — alerts are grouped by `alertname`; multiple instances of the same rule share one Live Activity
3. **Activity creation** — auto-creates a PushWard activity on the first firing instance of a new alert rule
4. **Firing** — sends an `ONGOING` update with the `alert` content template, worst severity icon/color, `generatorURL` link, and `startsAt` timestamp
5. **Partial resolve** — when some instances resolve but others remain firing, the activity updates to reflect the remaining instances
6. **Resolved** — when all instances resolve, sends an `ENDED` update with a green checkmark; the server auto-deletes after `ended_ttl` expires
7. **Stale timeout** — if a firing alert receives no updates for 24h, the server auto-ends it via `stale_ttl`
