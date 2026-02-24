# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

pushward-docker is a Go workspace containing integration bridges that connect external services to PushWard Live Activities on iOS. Each integration polls or listens to a service, then creates/updates/ends activities via the PushWard server API. The integrations are packaged as Docker containers and deployed independently.

## Build & Development Commands

```bash
# Build from repo root (Go workspace)
go build ./github/cmd/pushward-github
go build ./grafana/cmd/pushward-grafana
go build ./sabnzbd/cmd/pushward-sabnzbd
go build ./argocd/cmd/pushward-argocd

# Run locally
./pushward-github -config github/config.example.yml
./pushward-grafana -config grafana/config.example.yml
./pushward-sabnzbd -config sabnzbd/config.example.yml
./pushward-argocd -config argocd/config.example.yml

# Build Docker images (context is repo root for shared/ access)
docker build -f github/Dockerfile -t pushward-github .
docker build -f grafana/Dockerfile -t pushward-grafana .
docker build -f sabnzbd/Dockerfile -t pushward-sabnzbd .
docker build -f argocd/Dockerfile -t pushward-argocd .
```

There is no Makefile or dedicated lint config. CI uses `golangci-lint` via the shared reusable workflow.

### Testing

Only grafana, argocd, and github have unit tests; sabnzbd has none.

```bash
# Run tests for a specific integration (from repo root)
go test ./github/internal/poller/ -v -count=1
go test ./grafana/internal/handler/ -v -count=1
go test ./argocd/internal/handler/ -v -count=1

# With race detector (matches CI flags)
go test ./github/internal/poller/ -race -count=1 -v
go test ./grafana/internal/handler/ -race -count=1 -v
go test ./argocd/internal/handler/ -race -count=1 -v
```

All test files use shared test infrastructure from `shared/testutil`: `testutil.MockPushWardServer(t)` starts an `httptest.Server` recording all requests as `testutil.APICall{Method, Path, Body}`, `testutil.GetCalls()` retrieves recorded calls, and `testutil.UnmarshalBody()` decodes request bodies. Each test file has a local `testConfig()` returning a config with short timers (e.g. `EndDelay: 10ms`). Tests use `time.Sleep()` to wait for timer-driven async operations.

## Architecture

### Go workspace

`go.work` (Go 1.25) declares five modules: `./shared`, `./github`, `./grafana`, `./sabnzbd`, and `./argocd`. The `shared/` module contains the PushWard API client, common config types, HTTP server boilerplate, and test utilities. Each integration imports `shared/` and adds only integration-specific logic.

### Common patterns

All integrations follow the same internal structure:

- **`cmd/<name>/main.go`** -- entry point: loads config, creates API clients, runs the main loop with graceful shutdown (SIGINT/SIGTERM) via `shared/server`
- **`internal/config/`** -- YAML config with environment variable overrides (env vars take precedence); embeds `sharedconfig.PushWardConfig` and `sharedconfig.ServerConfig`
- **`internal/<service>/`** -- external service API client

The `shared/` module provides:
- **`shared/pushward/`** -- PushWard API client: `CreateActivity`, `UpdateActivity`, `DeleteActivity` with exponential backoff retry (up to 5 attempts) and 429 rate-limit handling (`Retry-After` header parsing with fallback to exponential backoff). Uses a superset `Content` struct with `omitempty` so unused fields don't appear in JSON.
- **`shared/config/`** -- Common `PushWardConfig` and `ServerConfig` types, `LoadYAML()`, `ApplyEnvOverrides()`, `Validate()`
- **`shared/server/`** -- `NewMux()` (registers `/health`), `ListenAndServe()` (graceful shutdown with `5*time.Second` timeout)
- **`shared/testutil/`** -- `MockPushWardServer`, `GetCalls`, `UnmarshalBody` for integration tests

All integrations use `log/slog` with JSON output to stdout. The PushWard client uses `http.Client{Timeout: 10s}` and handles two status codes specially: `409 Conflict` ("already exists" → success, "limit" → error) and `429 Too Many Requests` (retries with `Retry-After` header or exponential backoff). Other 4xx errors are not retried; 5xx errors are retried.

### Content fields

The shared `Content` struct is a superset of all template fields. Each integration uses only the relevant subset (unused fields are omitted via `omitempty`):

| Field | `pipeline` (github, argocd) | `alert` (grafana) | `generic` (sabnzbd) |
|---|---|---|---|
| Template, Progress, State, Icon, Subtitle, AccentColor | yes | yes | yes |
| CurrentStep, TotalSteps, StepRows | yes | -- | -- |
| URL, SecondaryURL | yes | yes | -- |
| Severity, FiredAt | -- | yes | -- |
| RemainingTime | -- | -- | yes |

### PushWard API integration

All integrations authenticate via `Authorization: Bearer hlk_...` (integration key with `activity:manage` scope). The activity lifecycle is:

1. `POST /activities` -- create activity (slug, name, priority, ended_ttl, stale_ttl)
2. `PATCH /activity/{slug}` -- update with state (`ONGOING`/`ENDED`) and content (template, progress, icon, subtitle, etc.)
3. Server auto-deletes after `ended_ttl` expires; `DELETE /activities/{slug}` retained on client but no longer called by bridges

Activity states drive push behavior on the server side:
- First `ONGOING` update triggers push-to-start (Live Activity appears on device)
- Subsequent `ONGOING` updates use push-update tokens
- `ENDED` update dismisses the Live Activity

## pushward-github

Polls GitHub Actions API for in-progress workflow runs and maps CI/CD pipeline progress to Live Activities.

### Key files

| File | Purpose |
|---|---|
| `github/cmd/pushward-github/main.go` | Entry point: config, clients, poller |
| `github/internal/config/config.go` | GitHub + PushWard config with `PUSHWARD_*` env overrides |
| `github/internal/github/client.go` | GitHub API client: workflow runs, jobs, repo discovery; retry with backoff (3 attempts) |
| `github/internal/github/types.go` | GitHub API response types (WorkflowRun, Job, Repository) |
| `github/internal/poller/poller.go` | Main poll loop: idle/active intervals, workflow tracking, cleanup |
| `github/internal/poller/state.go` | `trackedRun` struct (repo, run ID, slug, timestamps) |
| `github/config.example.yml` | Example configuration |

### Polling behavior

- **Idle**: polls all repos every 60s for in-progress workflow runs
- **Active**: when tracking a run, polls jobs on the next idle cycle and updates progress
- **Repo discovery**: if `github.owner` is set, auto-discovers repos every 5min (skips archived/disabled)
- **Stale timeout**: tracked workflows with no update for 30min are force-ended (server-side via `stale_ttl`)
- **One run per repo**: tracks the most recently created in-progress run per repo

### Content features

- **URL links**: each update includes `url` (workflow run HTMLURL) and `secondary_url` (repo link `https://github.com/<owner>/<repo>`)
- **Matrix job grouping**: `baseJobName()` strips matrix parameters (e.g. `"Build (ubuntu, node-16)"` → `"Build"`), groups parallel jobs into steps, and sends `step_rows` (e.g. `[1,1,3,1]` for a 3-job matrix at step 3)
- **Accent colors**: green while running, red on failure
- **Two-phase end**: `scheduleEnd()` sends ONGOING with final content (after `EndDelay`) then ENDED (after `EndDisplayTime`) to ensure Dynamic Island shows the completion state before dismissal. Server handles cleanup via `ended_ttl`.

### Activity slug format

`gh-<repo-name>` (e.g. `gh-pushward-server`). Uses the `pipeline` content template for all states.
Recommended `activity_slugs` prefix for integration key: `gh-*`

## pushward-sabnzbd

Exposes a webhook endpoint that SABnzbd calls when NZBs are added. Tracks download progress and post-processing phases as a Live Activity.

### Key files

| File | Purpose |
|---|---|
| `sabnzbd/cmd/pushward-sabnzbd/main.go` | Entry point: HTTP server (`:8090`), tracker, graceful shutdown |
| `sabnzbd/internal/config/config.go` | SABnzbd + PushWard config with `PUSHWARD_*` env overrides |
| `sabnzbd/internal/sabnzbd/client.go` | SABnzbd API client: `GetQueue`, `GetHistory` |
| `sabnzbd/internal/sabnzbd/types.go` | SABnzbd API response types (Queue, QueueSlot, History) |
| `sabnzbd/internal/tracker/tracker.go` | Core logic: webhook handler, download/PP tracking loop |
| `sabnzbd/config.example.yml` | Example configuration |

### Tracking flow

1. **Webhook** (`POST /webhook`) -- SABnzbd notifies on NZB added
2. **Wait** -- polls queue for up to 60s waiting for active download
3. **Download tracking** -- polls every 1s, shows progress, speed (MB/s), ETA, filename(s)
4. **Post-processing** -- tracks phases: Verifying, Repairing, Extracting, Moving (each with distinct icon)
5. **Queue continuation** -- if more downloads appear, loops back to step 3
6. **Summary** -- shows completed filename (or total size/duration/speed); two-phase end dismisses the Live Activity

### Content features

- **Accent colors**: blue during downloading/paused, orange during post-processing, green on completion
- **Two-phase end**: after completion, sends ONGOING with final content (after `EndDelay`) then ENDED (after `EndDisplayTime`). Skipped for resumed sessions (sends ENDED directly). Server handles cleanup via `ended_ttl`.

### Startup behavior

- **Resume**: on startup, checks SABnzbd for active downloads/post-processing and resumes tracking (skips cleanup delay)
- **Cleanup**: if no active work, sends `ENDED` to dismiss any stale activity from a previous crash

### Endpoints

| Method | Path | Description |
|---|---|---|
| POST | `/webhook` | Trigger download tracking (called by SABnzbd) |
| GET | `/health` | Health check (returns `ok`) |

### Activity slug

Fixed slug: `sabnzbd`. Uses the `generic` content template.
Recommended `activity_slugs` for integration key: `sabnzbd` (exact match)

## pushward-grafana

Receives Grafana alert webhooks, groups alerts by `alertname`, and creates/updates/ends Live Activities based on alert lifecycle.

### Key files

| File | Purpose |
|---|---|
| `grafana/cmd/pushward-grafana/main.go` | Entry point: HTTP server (`:8090`), webhook handler, graceful shutdown |
| `grafana/internal/config/config.go` | Grafana + PushWard config with `PUSHWARD_*` env overrides |
| `grafana/internal/grafana/types.go` | Grafana webhook payload types |
| `grafana/internal/handler/handler.go` | Alert grouping by alertname, firing/resolved lifecycle, stale/cleanup timers |
| `grafana/internal/handler/handler_test.go` | Full test coverage |
| `grafana/config.example.yml` | Example configuration |

### Behavior

- Groups multiple instances of the same alert rule into one Live Activity
- Worst severity (critical > warning > info) determines icon/color
- Stale timeout: 24h (server-side via `stale_ttl`); cleanup delay: 5m (server-side via `ended_ttl`)
- On resolved: sends ENDED directly (no two-phase end), removes group from local map

### Content features

- **URL links**: `url` from `generatorURL` (alert rule link in Grafana), `secondary_url` from `panelURL` or `dashboardURL` label
- **FiredAt**: `fired_at` unix timestamp from alert `startsAt`
- **Accent colors**: hex red (`#FF3B30`) for critical, hex orange (`#FF9500`) for warning, hex blue (`#007AFF`) for info, hex green (`#34C759`) for resolved

### Activity slug format

`grafana-<sha256(alertname)[:6]>`. Uses the `alert` content template.
Recommended `activity_slugs` prefix for integration key: `grafana-*`

## pushward-argocd

Receives ArgoCD sync webhooks (via argocd-notifications) and maps sync progress to a 3-step pipeline Live Activity: **Syncing -> Rolling Out -> Deployed**.

### Key files

| File | Purpose |
|---|---|
| `argocd/cmd/pushward-argocd/main.go` | Entry point: HTTP server (`:8090`), webhook handler, graceful shutdown |
| `argocd/internal/config/config.go` | ArgoCD + PushWard config with `PUSHWARD_*` env overrides |
| `argocd/internal/argocd/types.go` | ArgoCD webhook payload type |
| `argocd/internal/handler/handler.go` | Core state machine: event routing, app tracking, stale/cleanup timers |
| `argocd/internal/handler/handler_test.go` | Full test coverage |
| `argocd/config.example.yml` | Example configuration with argocd-notifications setup |

### Sync lifecycle

| ArgoCD Event | Step | State Text | PushWard State |
|---|---|---|---|
| `sync-running` | 1/3 | Syncing... | ONGOING |
| `sync-succeeded` | 2/3 | Rolling out... | ONGOING |
| `deployed` | 3/3 | Deployed | ENDED |
| `sync-failed` | current | Sync Failed | ENDED |
| `health-degraded` | current | Degraded | ENDED |

### Behavior

- Tracks apps independently by name, keyed on revision
- New revision during an active sync resets tracking and creates a new activity
- **Sync grace period** (default 10s): defers activity creation until the sync takes longer than the grace window. Syncs that complete within the window (no-op auto-syncs) are silently skipped. Errors (`sync-failed`, `health-degraded`) always bypass the grace period and create immediately.
- **Recent deploys tracking**: `deployed` events from untracked apps are recorded in `recentDeploys` (expires after `2 * SyncGracePeriod`). A subsequent `sync-running` checks this map and skips if it finds an entry (out-of-order event detection after bridge restart).
- Untracked events (bridge restart): `deployed` is skipped (noise), `sync-succeeded` enters grace period, errors always create immediately
- **Two-phase end**: `scheduleEnd()` sends `ONGOING` with final content first (after `EndDelay`, default 5s) to ensure iOS receives it via push-update token, then sends `ENDED` (after `EndDisplayTime`, default 4s) to dismiss. Server handles cleanup via `ended_ttl`.
- **Transient health-degraded**: when `health-degraded` arrives during step 2 (rolling out) on a tracked non-pending app, it sends an ONGOING update with orange warning icon instead of ending -- allowing `deployed` to complete the activity normally.
- Stale timeout: 30m (syncs shouldn't take longer)
- Webhook secret: optional `X-Webhook-Secret` header validation
- **ArgoCD URL**: if configured, activities include a "View in ArgoCD" link (`<url>/applications/argocd/<app>`) and a commit link (`repo_url/commit/<revision>`)

### Activity slug format

`argocd-<sanitized-app-name>` (e.g. `argocd-pushward-server`). Uses the `pipeline` content template.
Recommended `activity_slugs` prefix for integration key: `argocd-*`

### Endpoints

| Method | Path | Description |
|---|---|---|
| POST | `/webhook` | Receive argocd-notifications webhook |
| GET | `/health` | Health check (returns `ok`) |

## CI/CD

Each integration has a separate GitHub Actions workflow with path filters:

- `.github/workflows/github-ci-cd.yml` -- triggers on `github/**` and `shared/**` changes
- `.github/workflows/grafana-ci-cd.yml` -- triggers on `grafana/**` and `shared/**` changes
- `.github/workflows/sabnzbd-ci-cd.yml` -- triggers on `sabnzbd/**` and `shared/**` changes
- `.github/workflows/argocd-ci-cd.yml` -- triggers on `argocd/**` and `shared/**` changes

All use `mac-lucky/actions-shared-workflows/go-cicd-reusable.yml`. Triggers: push to `main`, tags (`v*`), pull requests to `main`, and manual `workflow_dispatch`. Docker images are built and pushed to GHCR (`ghcr.io/mac-lucky/pushward-{github,grafana,sabnzbd,argocd}`) on push to main or tags.

## Docker

All Dockerfiles use multi-stage builds: `golang:1.25-alpine` builder, `alpine:3.23` runtime. Build context is the repo root (`.`) so that both `shared/` and the integration source are accessible. Binaries are statically compiled (`CGO_ENABLED=0`, stripped with `-ldflags="-s -w"`). Runtime runs as non-root `appuser` (UID 1000). Default config path in containers: `/config/config.yml`.

Grafana, SABnzbd, and ArgoCD Dockerfiles expose port 8090. The GitHub Dockerfile does not expose any ports (polling-only, no HTTP server).

## Configuration

All settings support YAML config file (`-config` flag, default `config.yml`) and environment variable overrides. Env vars always take precedence.

### Common env vars (all integrations)

These are handled by `shared/config.PushWardConfig.ApplyEnvOverrides()`:

| Variable | Description |
|---|---|
| `PUSHWARD_URL` | PushWard server URL |
| `PUSHWARD_API_KEY` | Integration key (`hlk_` prefix, requires `activity:manage` scope) |
| `PUSHWARD_PRIORITY` | Activity priority 0-10 (defaults: github=1, sabnzbd=1, argocd=3, grafana=5) |
| `PUSHWARD_CLEANUP_DELAY` | Used as `ended_ttl` value passed to server on activity creation (defaults: github/sabnzbd=15m, grafana/argocd=5m) |
| `PUSHWARD_STALE_TIMEOUT` | Used as `stale_ttl` value passed to server on activity creation (defaults: github/sabnzbd/argocd=30m, grafana=24h) |
| `PUSHWARD_END_DELAY` | Wait before Phase 1 ONGOING in two-phase end (default: 5s; not used by grafana) |
| `PUSHWARD_END_DISPLAY_TIME` | Display time before ENDED in two-phase end (default: 4s; not used by grafana) |

### GitHub-specific env vars

| Variable | Description |
|---|---|
| `PUSHWARD_GITHUB_TOKEN` | GitHub PAT with `actions:read` scope |
| `PUSHWARD_GITHUB_OWNER` | GitHub username for auto-discovery |
| `PUSHWARD_GITHUB_REPOS` | Comma-separated `owner/repo` list |
| `PUSHWARD_POLL_IDLE` | Idle poll interval (default: 60s) |

### SABnzbd-specific env vars

| Variable | Description |
|---|---|
| `PUSHWARD_SABNZBD_URL` | SABnzbd API URL |
| `PUSHWARD_SABNZBD_API_KEY` | SABnzbd API key |
| `PUSHWARD_SERVER_ADDRESS` | HTTP listen address (default: `:8090`) |
| `PUSHWARD_POLL_INTERVAL` | Poll interval during tracking (default: 1s) |

### Grafana-specific env vars

| Variable | Description |
|---|---|
| `PUSHWARD_SERVER_ADDRESS` | HTTP listen address (default: `:8090`) |
| `PUSHWARD_GRAFANA_SEVERITY_LABEL` | Alert label name to read severity from (default: `severity`) |
| `PUSHWARD_GRAFANA_DEFAULT_SEVERITY` | Fallback severity when label is missing (default: `warning`) |
| `PUSHWARD_GRAFANA_DEFAULT_ICON` | Fallback icon (default: `exclamationmark.triangle.fill`) |

### ArgoCD-specific env vars

| Variable | Description |
|---|---|
| `PUSHWARD_ARGOCD_WEBHOOK_SECRET` | Optional webhook secret for request validation |
| `PUSHWARD_ARGOCD_URL` | ArgoCD UI base URL; enables "View in ArgoCD" link on activities |
| `PUSHWARD_SERVER_ADDRESS` | HTTP listen address (default: `:8090`) |
| `PUSHWARD_SYNC_GRACE_PERIOD` | Skip no-op syncs completing within this window (default: 10s, 0 to disable) |
