# pushward-grafana

Bridges Grafana alert notifications to [PushWard](https://github.com/mac-lucky/pushward-server) Live Activities on iOS.

Exposes a webhook endpoint that Grafana calls via its contact point configuration. Firing alerts appear as Live Activities on your iPhone's Dynamic Island and Lock Screen, color-coded by severity. Resolved alerts automatically dismiss.

## Features

- **Severity-based styling** — critical (red), warning (orange), and info (blue) alerts each get a distinct icon and accent color
- **Auto-resolved** — resolved alerts show a green checkmark and dismiss after the cleanup delay
- **Stale detection** — alerts with no updates for 24h are auto-ended to prevent orphaned Live Activities
- **Fingerprint-based slugs** — each unique alert gets its own Live Activity (`grafana-<fingerprint>`)
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
| `PUSHWARD_SERVER_ADDRESS` | `server.address` | HTTP listen address (default: `:8090`) | No |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Activity priority 0-10 (default: `5`) | No |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | Delay before deleting ended activities (default: `5m`) | No |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Auto-end alerts with no updates after this duration (default: `24h`) | No |

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
2. **Activity creation** — auto-creates a PushWard activity for each unique alert fingerprint
3. **Firing** — sends an `ONGOING` update with the `alert` content template, severity-based icon and color
4. **Resolved** — sends an `ENDED` update with a green checkmark, then deletes after the cleanup delay
5. **Stale timeout** — if a firing alert receives no updates for 24h, it is auto-ended and cleaned up
