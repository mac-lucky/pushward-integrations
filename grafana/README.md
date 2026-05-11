# pushward-grafana

Bridges Grafana alerts to [PushWard](https://pushward.app) Live Activity timelines on iOS, with Prometheus / VictoriaMetrics history backfill and live sparklines.

Receives Grafana webhook alerts on `/webhook`, queries the metrics backend for the firing series' history, and pushes a timeline activity that updates as the metric moves — visible on the Dynamic Island and Lock Screen.

Also publishes scalar / gauge / progress / stat-list **widgets** polled directly from Prometheus, surfaced on the iOS Home Screen and Lock Screen widgets.

## Features

- **Timeline sparklines** — alert series rendered as a live sparkline with thresholds, units, and per-series labels
- **History backfill** — on first firing, queries `history_window` (default 30m) of points so the sparkline isn't empty
- **Multi-series support** — alerts that produce multiple results fan out into a labeled timeline (one line per series); keys stay stable across firing → resolved so accumulated history isn't pruned
- **Multi-instance tracking** — alerts firing from multiple Alertmanager / Prometheus instances are tracked by fingerprint; the activity ends only when *all* instances resolve
- **Auto query extraction** — when a Grafana service-account token is configured, PromQL is pulled directly from the alert rule definition (no per-rule annotations needed)
- **Alert state checker** — background goroutine that polls Grafana's alertmanager API on `alert_check_interval` (default 5m) so missed `resolved` webhooks still close out the activity
- **Stale sweeper** — in-memory state for unresolved alerts is dropped after `stale_timeout` to prevent unbounded growth
- **Webhook auth** — optional `Authorization: Bearer <token>` validation via `webhook_token`
- **Resolved styling** — resolved alerts switch icon and accent color (green ✓) before the lock-screen dismissal grace period
- **Widgets** — scalar `value` / `progress` / `gauge` / `status` / `stat_list` widgets polled from PromQL on a per-widget interval, with multi-series fan-out via `query_all` + `slug_template`
- **Active alert cap** — caps in-memory tracking at 500 concurrent alerts; over-cap firings are dropped with a rate-limited warning
- **Retry with backoff** — PushWard API calls retry up to 5× with exponential backoff
- **Graceful shutdown** — drains in-flight webhook processing on SIGINT/SIGTERM

## Prerequisites

- A running PushWard server
- A PushWard integration key (`hlk_` prefix); add the `widgets` scope if publishing widgets
- A Prometheus or VictoriaMetrics endpoint reachable from the bridge
- A Grafana instance configured to send alert webhooks
- (Optional) A Grafana service-account token (Editor role) for auto query extraction + alert state checks
- The PushWard iOS app subscribed to the relevant activity slugs

## Configuration

All settings can be provided via YAML config or environment variables. Environment variables take precedence.

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_URL` | `pushward.url` | PushWard server URL | Yes |
| `PUSHWARD_API_KEY` | `pushward.api_key` | PushWard integration key (`hlk_`) | Yes |
| `PUSHWARD_METRICS_URL` | `metrics.url` | Prometheus / VictoriaMetrics base URL | Yes |
| `PUSHWARD_METRICS_USERNAME` | `metrics.username` | Basic-auth username for the metrics backend | No |
| `PUSHWARD_METRICS_PASSWORD` | `metrics.password` | Basic-auth password | No |
| `PUSHWARD_METRICS_BEARER_TOKEN` | `metrics.bearer_token` | Bearer token for the metrics backend | No |
| `PUSHWARD_METRICS_TIMEOUT` | `metrics.timeout` | Per-query timeout (e.g. `5s`) | No |
| `PUSHWARD_GRAFANA_URL` | `grafana.url` | Grafana base URL (enables auto query extraction + alert state checks) | No |
| `PUSHWARD_GRAFANA_API_TOKEN` | `grafana.api_token` | Grafana service-account token (Editor role) | No |
| `PUSHWARD_ALERT_CHECK_INTERVAL` | `grafana.alert_check_interval` | Poll Grafana for missed `resolved` webhooks (default off; e.g. `5m`) | No |
| `PUSHWARD_WEBHOOK_TOKEN` | `webhook_token` | Required `Authorization: Bearer <token>` on `/webhook` | No (recommended) |
| `PUSHWARD_SERVER_ADDRESS` | `server.address` | HTTP listen address (default `:8090`) | No |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Activity priority 0-10 (default `5`) | No |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | Server-side `ended_ttl`: time after resolve before the activity row is deleted **and** the iOS Lock Screen entry is dismissed (default `15m`; Apple caps Lock Screen dismissal at 4h) | No |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Time before the in-memory sweeper drops an unresolved alert (default `24h`) | No |
| `PUSHWARD_HISTORY_WINDOW` | `timeline.history_window` | How far back to backfill on initial firing (default `30m`) | No |
| `PUSHWARD_POLL_INTERVAL` | `timeline.poll_interval` | How often the per-alert poller refreshes data points (default `30s`) | No |
| `PUSHWARD_WIDGETS_JSON` | _(see below)_ | Full widget list as JSON; replaces YAML `widgets` wholesale (Helm-friendly) | No |

> **`cleanup_delay` semantics.** The bridge passes this value to PushWard as `ended_ttl` at create time. On resolve the server uses it to set the APNs `dismissal-date`, which controls when iOS removes the Live Activity from the Lock Screen. Past 4 hours, Apple silently caps it. Set it to the grace period you want users to see the "resolved" state before it auto-dismisses.

See [`config.example.yml`](./config.example.yml) for the full timeline / widget schema.

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
      - PUSHWARD_METRICS_URL=http://prometheus:9090
      - PUSHWARD_WEBHOOK_TOKEN=change-me
      # Optional auto-extraction + missed-resolve recovery:
      - PUSHWARD_GRAFANA_URL=http://grafana:3000
      - PUSHWARD_GRAFANA_API_TOKEN=glsa_xxxxxxxxxxxx
      - PUSHWARD_ALERT_CHECK_INTERVAL=5m
```

## Grafana Webhook Setup

In Grafana, go to **Alerting → Contact points → New contact point**:

- **Integration:** Webhook
- **URL:** `http://<pushward-grafana-host>:8090/webhook`
- **HTTP Method:** POST
- **Authorization Header — Scheme:** `Bearer`
- **Authorization Header — Credentials:** the value of `PUSHWARD_WEBHOOK_TOKEN`

Then route your alert rules (or a notification policy) to that contact point. The bridge handles both `firing` and `resolved` payloads from the same contact point.

## Endpoints

| Method | Path | Description |
|---|---|---|
| POST | `/webhook` | Grafana alert webhook (firing + resolved) |
| GET | `/health` | Liveness probe (returns `ok`) |
| GET | `/metrics` | Prometheus metrics for the bridge itself |

## Per-rule Annotations

Add these annotations on a Grafana alert rule to control the resulting timeline:

| Annotation | Purpose |
|---|---|
| `pushward_query` | PromQL expression for history backfill and ongoing polling. Required if Grafana auto-extraction is disabled. |
| `pushward_ref_id` | Which `values{}` key (Grafana ref ID like `B`, `C`) drives the value when an alert reports multiple. |
| `pushward_unit` | Unit label rendered next to the value (e.g. `%`, `°C`, `ms`). |
| `pushward_threshold` | Numeric threshold rendered as a dashed line on the sparkline. Leading comparator (`> `, `>=`) is stripped. |
| `pushward_series_label` | Which Prometheus label to use as the series key when fanning out multi-series alerts (e.g. `instance`). |
| `summary` | Used as the activity's `state` text. Falls back to `alertname`. |
| `severity` (label) | Drives icon and accent color: `critical` / `warning` / `info`. The label name is configurable via `severity_label`. |

## How It Works

1. **Webhook in** — Grafana POSTs the alert group to `/webhook`. The bridge responds 200 immediately and processes asynchronously so slow Prometheus queries don't trigger Grafana retries.
2. **Firing** — for a new `alertname`, the bridge:
   - Reserves a slot under the active-alerts cap
   - Resolves PromQL (annotation → auto-extracted from rule via Grafana API)
   - Calls `CreateActivity` on PushWard with `priority`, `cleanup_delay` (as `ended_ttl`), and `stale_timeout`
   - Backfills `history_window` of points and seeds the timeline content
   - Starts a per-alert poller that refreshes values every `poll_interval`
3. **Re-fire** — subsequent firings of the same `alertname` register their fingerprint on the existing tracked alert without recreating the activity.
4. **Resolved** — drops the resolving fingerprint. When all fingerprints for an `alertname` are gone:
   - Stops the poller
   - Sends one final `UpdateActivity` with `StateEnded`, switching the icon to the green ✓ and re-using the last poller series keys so the server preserves accumulated history
   - The server then sends an APNs end push with `dismissal-date = now + ended_ttl` (your `cleanup_delay`)
5. **Missed-resolve recovery** — if `grafana.url` + `grafana.api_token` + `alert_check_interval` are configured, a background goroutine queries Grafana's alertmanager API and ends activities for alerts that are no longer firing.
6. **Stale sweep** — alerts that never receive a `resolved` and aren't covered by the alert state checker are dropped from the in-memory map after `stale_timeout` (poller stopped; activity is left for `stale_ttl` server-side cleanup).

## Widgets

Widgets are independent of alerts: each entry in the `widgets` list (or `PUSHWARD_WIDGETS_JSON`) declares a slug, a PromQL query, an interval, and an iOS widget template. The bridge:

- Calls `CreateWidget` on startup (idempotent — server upserts on slug)
- Polls each widget on its own ticker
- PATCHes the value when it moves (or every tick if `update_mode: always`)
- For `query_all`, fans the result rows into one widget per series via `slug_template` / `name_template`
- For `stat_list`, polls each row's query independently and renders the value through the row's `value_template`

The integration key must have the `widgets` scope, otherwise the server returns 403 on first create.
