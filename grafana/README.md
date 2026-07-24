[![Website](https://img.shields.io/badge/pushward.app-5B4FE5?style=for-the-badge&logo=safari&logoColor=white)](https://pushward.app)
[![App Store](https://img.shields.io/badge/App_Store-Download-0D96F6?style=for-the-badge&logo=apple&logoColor=white)](https://apps.apple.com/app/id6759689999)

# PushWard for Grafana

[![CI/CD Grafana](https://github.com/mac-lucky/pushward-integrations/actions/workflows/grafana-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/grafana-ci-cd.yml)
[![Image](https://img.shields.io/badge/ghcr.io-pushward--grafana-2496ED?logo=docker&logoColor=white)](https://github.com/mac-lucky/pushward-integrations/pkgs/container/pushward-grafana)

Turns Grafana alerts into [PushWard](https://pushward.app) **Live Activity timelines** on iPhone — a live sparkline of the firing metric on the Dynamic Island and Lock Screen, backfilled from Prometheus / VictoriaMetrics history and updated as the metric moves. It can also poll PromQL on a schedule and publish the results as PushWard **iOS Home / Lock Screen widgets**.

> **New to PushWard?** Learn what it is at **[pushward.app](https://pushward.app)** and download the app from the **[App Store](https://apps.apple.com/app/id6759689999)**.

## How it works

```
Grafana alert ──POST /webhook──▶ pushward-grafana ──query──▶ Prometheus / VictoriaMetrics
                                       │
                                       └──REST──▶ pushward-server ──APNs──▶ iOS Live Activity
```

Grafana POSTs a firing alert to `POST /webhook`. The bridge resolves the series' PromQL (from per-rule annotations, or auto-extracted via the Grafana API), backfills history from your metrics backend, and calls the PushWard server to create a "timeline" activity. A per-alert poller refreshes the sparkline on an interval until the alert resolves, then ends the activity. Widgets follow a separate path: each declared widget is polled from PromQL on its own ticker and published to the server widget API.

## Features

- **Timeline sparklines** — the firing series is rendered as a live sparkline with threshold line, unit, and per-series labels.
- **History backfill** — on first firing, the bridge queries `history_window` (default `30m`) of points so the sparkline isn't empty (step is `history_window/120`, floored at 15s).
- **Multi-series fan-out** — alerts that return multiple results render as a labeled timeline (one line per series); series keys stay stable across firing → resolved so accumulated history isn't pruned.
- **Multi-instance tracking** — an alert firing from multiple instances is grouped by `alertname` and tracked by fingerprint; the activity ends only when *all* fingerprints resolve.
- **Auto query extraction** — when a Grafana service-account token is set, PromQL is pulled straight from the alert rule definition, so no per-rule annotations are needed.
- **Missed-resolve recovery** — an optional background goroutine polls Grafana's alertmanager API on `alert_check_interval` to close out activities whose `resolved` webhook was dropped.
- **Severity styling** — `critical` / `warning` / `info` drive the activity icon and accent color; resolved alerts switch to a green checkmark before dismissal.
- **Widgets** — `value` / `progress` / `status` / `gauge` / `stat_list` widgets polled from PromQL, with multi-series fan-out via `query_all` + `slug_template`.
- **Self-protecting** — webhooks are answered immediately and processed asynchronously (30s budget); in-memory tracking is capped at 500 active alerts and swept for stale entries.

## Prerequisites

- A running **PushWard server** (public production base: `https://api.pushward.app`).
- A PushWard **integration key** (`hlk_` prefix). Publishing widgets additionally requires the key's **`widgets` scope** (the server returns `403` on the first widget create otherwise).
- A **Prometheus or VictoriaMetrics** endpoint reachable from the bridge — queried directly for series history and instant values.
- A **Grafana** instance configured to send alert webhooks to this bridge.
- *(Optional)* A **Grafana service-account token** (Editor role) to enable PromQL auto-extraction and missed-resolve recovery.
- The **PushWard iOS app** installed and subscribed to the relevant activity slugs.

## Quickstart (Docker)

The image runs as non-root (UID 1000), listens on `:8090`, and reads `/config/config.yml` by default.

```bash
docker run -p 8090:8090 \
  -e PUSHWARD_URL=https://api.pushward.app \
  -e PUSHWARD_API_KEY=YOUR_API_KEY \
  -e PUSHWARD_METRICS_URL=http://prometheus:9090 \
  -e PUSHWARD_WEBHOOK_TOKEN=change-me \
  ghcr.io/mac-lucky/pushward-grafana:latest
```

Or with Docker Compose and a mounted config file:

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
      - PUSHWARD_API_KEY=YOUR_API_KEY
      - PUSHWARD_METRICS_URL=http://prometheus:9090
      - PUSHWARD_WEBHOOK_TOKEN=change-me
      # Optional: PromQL auto-extraction + missed-resolve recovery
      - PUSHWARD_GRAFANA_URL=http://grafana:3000
      - PUSHWARD_GRAFANA_API_TOKEN=YOUR_GRAFANA_TOKEN
      - PUSHWARD_ALERT_CHECK_INTERVAL=5m
```

A starting `config.yml` lives in [`config.example.yml`](./config.example.yml).

## Grafana webhook setup

In Grafana, go to **Alerting → Notification configuration → Contact points**, then add a new contact point:

- **Integration:** Webhook
- **URL:** `http://<pushward-grafana-host>:8090/webhook`
- **HTTP Method:** `POST`
- **Authorization Header — Scheme:** `Bearer`
- **Authorization Header — Credentials:** the value of `webhook_token` / `PUSHWARD_WEBHOOK_TOKEN`

Then route your alert rules (or a notification policy) to that contact point. The same contact point handles both `firing` and `resolved` payloads.

## Configuration

Settings come from a YAML config file **or** environment variables. **Env vars override YAML**, and the standardized prefix is `PUSHWARD_*`. Three values are required and the bridge refuses to start without them.

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_URL` | `pushward.url` | PushWard server base URL (e.g. `https://api.pushward.app`) | **Yes** |
| `PUSHWARD_API_KEY` | `pushward.api_key` | Integration key (`hlk_` prefix); needs `widgets` scope to publish widgets | **Yes** |
| `PUSHWARD_METRICS_URL` | `metrics.url` | Prometheus / VictoriaMetrics base URL | **Yes** |
| `PUSHWARD_METRICS_USERNAME` | `metrics.username` | Basic-auth username for the metrics backend (enables basic auth when set) | No |
| `PUSHWARD_METRICS_PASSWORD` | `metrics.password` | Basic-auth password for the metrics backend | No |
| `PUSHWARD_METRICS_BEARER_TOKEN` | `metrics.bearer_token` | Bearer token for the metrics backend (enables bearer auth when set) | No |
| `PUSHWARD_METRICS_TIMEOUT` | `metrics.timeout` | Per-query timeout for the metrics backend (e.g. `5s`); overrides the built-in 30s default, but only when set to a positive value | No |
| `PUSHWARD_GRAFANA_URL` | `grafana.url` | Grafana base URL — set with `api_token` to enable auto-extraction + missed-resolve recovery | No |
| `PUSHWARD_GRAFANA_API_TOKEN` | `grafana.api_token` | Grafana service-account token (Editor role) | No |
| `PUSHWARD_ALERT_CHECK_INTERVAL` | `grafana.alert_check_interval` | How often to poll Grafana for missed `resolved` webhooks; disabled when `0`/unset (needs `grafana.url` + `api_token`) | No |
| `PUSHWARD_WEBHOOK_TOKEN` | `webhook_token` | Shared secret; when set, `/webhook` requires `Authorization: Bearer <token>`. **Recommended** — endpoint is unauthenticated if unset | No |
| `PUSHWARD_SERVER_ADDRESS` | `server.address` | HTTP listen address (default `:8090`) | No |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Activity priority, validated to `0`–`10` (default `5`) | No |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | Sent to the server as `ended_ttl`: grace period after resolve before the activity row is deleted and the iOS Lock Screen entry is dismissed (default `15m`; Apple caps Lock Screen dismissal at 4h) | No |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Time before the in-memory sweeper drops an unresolved alert; also passed to the server (default `24h`; sweeper ticks every `stale_timeout/2`) | No |
| `PUSHWARD_HISTORY_WINDOW` | `timeline.history_window` | How far back to backfill series history on initial firing (default `30m`) | No |
| `PUSHWARD_POLL_INTERVAL` | `timeline.poll_interval` | How often the per-alert poller refreshes data points (default `30s`) | No |
| `PUSHWARD_WIDGETS_JSON` | `widgets` | Full widget list as JSON; **replaces** the YAML `widgets:` list wholesale (Helm-friendly). `interval` is a duration string like `"60s"` | No |

> **`cleanup_delay` semantics.** The bridge passes this value to the server as `ended_ttl` at create time. On resolve the server uses it to set the APNs `dismissal-date`, which controls when iOS removes the Live Activity from the Lock Screen. Beyond 4 hours Apple silently caps it.

### Timeline visual settings (YAML only)

These tune the sparkline rendering. They have **no environment-variable override** — set them under `timeline:` in the config file.

| Config Key | Description | Default |
|---|---|---|
| `timeline.smoothing` | Curve smoothing on the sparkline | `true` |
| `timeline.scale` | Y-axis scale: `linear` or `logarithmic` | `linear` |
| `timeline.decimals` | Value precision (decimal places) | `1` |
| `timeline.severity_label` | Which alert label drives severity (icon / color) | `severity` |
| `timeline.default_severity` | Severity used when the label is absent / unrecognized (`critical` / `warning` / `info`) | `warning` |

> **Note:** the shared `ServerConfig` also exposes `server.metrics_address` / `PUSHWARD_SERVER_METRICS_ADDRESS`, but this bridge starts no metrics server, so setting it has no effect.

## Per-rule annotations

Add these annotations to a Grafana alert rule to control the resulting timeline. If Grafana auto-extraction is enabled (`grafana.url` + `api_token`), `pushward_query` is optional — the bridge pulls the expression from the rule itself.

| Annotation | Purpose |
|---|---|
| `pushward_query` | PromQL for history backfill and polling. Required when auto-extraction is off. |
| `pushward_ref_id` | Which Grafana `values{}` key (ref ID like `B`, `C`) drives the value when an alert reports several. |
| `pushward_unit` | Unit rendered next to the value (e.g. `%`, `°C`, `ms`). |
| `pushward_threshold` | Numeric threshold drawn as a dashed line; leading comparator chars (`> < ! =`) are stripped. |
| `pushward_series_label` | Prometheus label used as the series key when fanning out a multi-series alert (e.g. `instance`). |
| `summary` | Used as the activity's state text. Falls back to the `alertname` label. |

Severity is read from the configured severity label (default `severity`); only `critical` / `warning` / `info` are honored, otherwise `default_severity` applies.

## Widgets

Widgets are **independent of alerts**. Each entry in the `widgets:` list (or `PUSHWARD_WIDGETS_JSON`) is polled from PromQL on its own ticker and published to the server widget API. They are created on startup (idempotent — the server upserts on slug) and PATCHed only when the value changes, unless `update_mode: always`.

| Template | Query field | Notes |
|---|---|---|
| `value` | `query` | Scalar number. |
| `status` | `query` | Scalar; renders as a status chip. |
| `progress` | `query` | Scalar; requires `content.min_value` + `content.max_value`. |
| `gauge` | `query` | Scalar; requires `content.min_value` + `content.max_value`. |
| `stat_list` | per-row `query` | 1–6 `stat_rows`, each with its own `query` + `value_template`. |

Multi-series fan-out: set `query_all` instead of `query` plus a required `slug_template` to publish one widget per result series.

Server-mirrored validation runs at config load: `interval` defaults to `60s` and must be `≥ 5s`; `update_mode` is `on_change` (default) or `always`; slug must match `^[a-z0-9_-]{1,128}$`; `stat_list` allows at most 6 rows, with row label ≤ 32 chars and row unit ≤ 16 chars. The per-user widget cap on the server is 50.

```yaml
widgets:
  # Scalar value
  - slug: "registered-users"
    name: "Registered Users"
    template: "value"
    query: "myapp_users_total"
    interval: 60s
    content:
      icon: "person.3.fill"
      unit: "users"

  # Gauge (requires min/max)
  - slug: "node-cpu"
    name: "Node CPU"
    template: "gauge"
    query: 'avg(100 - rate(node_cpu_seconds_total{mode="idle"}[1m]) * 100)'
    interval: 30s
    content:
      icon: "cpu"
      unit: "%"
      min_value: 0
      max_value: 100

  # Multi-series fan-out: one widget per instance
  - slug: "node-mem-base"
    query_all: 'node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes'
    slug_template: "node-mem-{{.instance}}"
    name_template: "Memory free on {{.instance}}"
    template: "progress"
    content:
      icon: "memorychip"
      min_value: 0
      max_value: 1
```

See [`config.example.yml`](./config.example.yml) for the full widget schema (including `stat_list` rows and the per-row `trigger` flag).

## Endpoints

| Method | Path | Description |
|---|---|---|
| `POST` | `/webhook` | Grafana alert webhook (firing + resolved). Responds `200` immediately, processes asynchronously (30s budget); body capped at 1 MiB. Requires `Authorization: Bearer <token>` when `webhook_token` is set. |
| `GET` | `/health` | Liveness probe — returns `200 ok`. |
| `GET` | `/ready` | Readiness probe — returns `200 ready` (this bridge registers no readiness checks, so it is always ready). |

There is no `/metrics` endpoint — this bridge exposes no Prometheus metrics of its own.

## Development

This bridge is a module in the `pushward-integrations` Go workspace (`go.work`). Run these from the **workspace root** so the workspace and the sibling `shared/` module resolve.

```bash
# Build
go build -o pushward-grafana ./grafana/cmd/pushward-grafana

# Run with a config file (the -config flag defaults to config.yml)
./pushward-grafana -config grafana/config.example.yml

# Tests (race detector + verbose, matches CI)
go test ./grafana/... -race -count=1 -v

# Lint (matches CI)
golangci-lint run

# Docker build — context is the repo root so the Dockerfile can COPY shared/
docker build -f grafana/Dockerfile -t pushward-grafana .

# Override the Go toolchain at build time if needed
docker build -f grafana/Dockerfile --build-arg GO_VERSION=1.26.5 -t pushward-grafana .
```

> The module targets `go 1.26.5` (`go.mod`), matching the Dockerfile's default `GO_VERSION` build arg (`1.26.5`); override it with `--build-arg GO_VERSION=<x.y.z>` only if you need a different toolchain.

## CI/CD & Releases

CI runs via [`grafana-ci-cd.yml`](../.github/workflows/grafana-ci-cd.yml) on PRs and pushes to `main` (path-filtered to `grafana/**` and `shared/**`). Images publish to **GHCR only** — `ghcr.io/mac-lucky/pushward-grafana` (a Docker Hub name is configured but `push_to_dockerhub` is `false`).

| Trigger | GHCR tags | Purpose |
|---|---|---|
| Pull request | _(none)_ | Tests + analysis only |
| Push to `main` | `:main`, `:main-<short-sha>` | Rolling unstable + immutable per-commit pin |
| Git tag `grafana/v<X.Y.Z>` | `:X.Y.Z`, `:X.Y`, `:latest` (and `:X` once `X ≥ 1`) | Stable release |

`:latest` only moves on a tagged release — never on a `main` push. Bridges are versioned independently; tag format is `grafana/v<X.Y.Z>`:

```bash
git tag grafana/v0.4.1
git push origin grafana/v0.4.1
```

The tag triggers [`release.yml`](../.github/workflows/release.yml), which runs the release pipeline for this bridge and produces a GitHub Release with an auto-generated changelog (categorized via `.github/release.yml`).

## Server compatibility

This bridge calls the [pushward-server](https://pushward.app) REST API to create / update / end **activities** (`POST /activities`, `PATCH /activities/{slug}`) and to publish **widgets** (`POST /widgets`, `PATCH /widgets/{slug}`, `DELETE /widgets/{slug}`). The wire contract — routes, JSON keys and casing, and auth headers — is owned by pushward-server's `openapi.yaml`. Bridges target the server API surface at their `MAJOR.MINOR`; a patch release is a bridge-only fix. Released iOS clients can't be hot-fixed, so the server contract is the binding compatibility surface.

## Troubleshooting

Logs are structured JSON on stdout (`docker logs pushward-grafana`). The process logs `webhook bearer auth enabled` or a warning that the endpoint is unauthenticated, and `grafana auto-extract enabled` when the Grafana API is wired up — check those lines first.

| Symptom | Likely cause / fix |
|---|---|
| Process exits immediately on start | A required value is missing: `pushward.url` / `PUSHWARD_URL`, `pushward.api_key` / `PUSHWARD_API_KEY`, or `metrics.url` / `PUSHWARD_METRICS_URL`. |
| `403` on first widget create | The integration key lacks the `widgets` scope. Use a key with widget capability. |
| Empty sparkline / no history | `pushward_query` is missing and auto-extraction is off (set `grafana.url` + `grafana.api_token`), or the metrics backend can't run the PromQL. Verify `metrics.url` and any `metrics.username` / `bearer_token` auth. |
| Webhook accepted (`200`) but no activity appears | Processing is async — check logs for the firing alert. Confirm the alert has a non-empty fingerprint and that the activity slug is subscribed in the iOS app. |
| Activity never ends | The `resolved` webhook was dropped. Enable missed-resolve recovery with `grafana.url` + `grafana.api_token` + `alert_check_interval`; otherwise the stale sweeper closes it out after `stale_timeout`. |
| `401`/`403` from Grafana to the bridge, or vice-versa | The `webhook_token` in the contact point's `Authorization: Bearer` header must match `PUSHWARD_WEBHOOK_TOKEN`; the Grafana service-account token must have Editor role. |
| Firing alerts silently dropped | The in-memory active-alert cap (500) was hit; a rate-limited warning is logged. Reduce alert volume or tune grouping. |

## Requirements & License

- Go 1.26.x toolchain (for building from source); Docker for the container image.
- A running PushWard server, an `hlk_` integration key, a Prometheus/VictoriaMetrics endpoint, and the PushWard iOS app.

Part of the public [pushward-integrations](https://github.com/mac-lucky/pushward-integrations) repository.
