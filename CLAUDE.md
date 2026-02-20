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

# Build Docker images
docker build -f github/Dockerfile -t pushward-github ./github
docker build -f grafana/Dockerfile -t pushward-grafana ./grafana
docker build -f sabnzbd/Dockerfile -t pushward-sabnzbd ./sabnzbd
docker build -f argocd/Dockerfile -t pushward-argocd ./argocd
```

There is no Makefile or dedicated lint config. CI uses `golangci-lint` via the shared reusable workflow.

## Architecture

### Go workspace

`go.work` (Go 1.25) declares four modules: `./github`, `./grafana`, `./sabnzbd`, and `./argocd`. Each module is self-contained with its own `go.mod`, `Dockerfile`, and CI workflow. The only shared dependency is `gopkg.in/yaml.v3`.

### Common patterns

All integrations follow the same internal structure:

- **`cmd/<name>/main.go`** -- entry point: loads config, creates API clients, runs the main loop with graceful shutdown (SIGINT/SIGTERM)
- **`internal/config/`** -- YAML config with environment variable overrides (env vars take precedence)
- **`internal/pushward/`** -- PushWard API client: `CreateActivity`, `UpdateActivity`, `DeleteActivity` with exponential backoff retry (up to 5 attempts). Each integration has its own copy with slightly different `Content` fields (e.g. sabnzbd adds `RemainingTime`)
- **`internal/<service>/`** -- external service API client

### PushWard API integration

All integrations authenticate via `Authorization: Bearer hlk_...` (integration key). The activity lifecycle is:

1. `POST /activities` -- create activity (slug, name, priority)
2. `PATCH /activity/{slug}` -- update with state (`ONGOING`/`ENDED`) and content (template, progress, icon, subtitle, etc.)
3. `DELETE /activities/{slug}` -- cleanup after delay

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
| `github/internal/pushward/client.go` | PushWard API client (create/update/delete with retry) |
| `github/config.example.yml` | Example configuration |

### Polling behavior

- **Idle**: polls all repos every 60s for in-progress workflow runs
- **Active**: when tracking a run, polls jobs every 5s and updates progress
- **Repo discovery**: if `github.owner` is set, auto-discovers repos every 5min (skips archived/disabled)
- **Cleanup**: after configurable delay (default 15min), deletes ended activities
- **Stale timeout**: tracked workflows with no update for 30min are force-ended
- **One run per repo**: tracks the most recently created in-progress run per repo

### Activity slug format

`gh-<repo-name>` (e.g. `gh-pushward-server`). Uses the `pipeline` content template for all states.

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
| `sabnzbd/internal/pushward/client.go` | PushWard API client (create/update/delete with retry) |
| `sabnzbd/config.example.yml` | Example configuration |

### Tracking flow

1. **Webhook** (`POST /webhook`) -- SABnzbd notifies on NZB added
2. **Wait** -- polls queue for up to 60s waiting for active download
3. **Download tracking** -- polls every 1s, shows progress, speed (MB/s), ETA, filename(s)
4. **Post-processing** -- tracks phases: Verifying, Repairing, Extracting, Moving (each with distinct icon)
5. **Queue continuation** -- if more downloads appear, loops back to step 3
6. **Summary** -- shows total size, duration, avg speed; waits cleanup delay (default 15min) then ends

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
| `grafana/internal/pushward/client.go` | PushWard API client (create/update/delete with retry) |
| `grafana/config.example.yml` | Example configuration |

### Behavior

- Groups multiple instances of the same alert rule into one Live Activity
- Worst severity (critical > warning > info) determines icon/color
- Stale timeout: 24h (force-ends stale alerts)
- Cleanup delay: 5m

### Activity slug format

`grafana-<sha256(alertname)[:6]>`. Uses the `alert` content template.

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
| `argocd/internal/pushward/client.go` | PushWard API client (create/update/delete with retry) |
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
- Untracked events (bridge restart) are handled gracefully: creates activity and proceeds
- Stale timeout: 30m (syncs shouldn't take longer)
- Cleanup delay: 5m
- Webhook secret: optional `X-Webhook-Secret` header validation

### Activity slug format

`argocd-<sanitized-app-name>` (e.g. `argocd-pushward-server`). Uses the `pipeline` content template.

### Endpoints

| Method | Path | Description |
|---|---|---|
| POST | `/webhook` | Receive argocd-notifications webhook |
| GET | `/health` | Health check (returns `ok`) |

## CI/CD

Each integration has a separate GitHub Actions workflow with path filters:

- `.github/workflows/github-ci-cd.yml` -- triggers on `github/**` changes
- `.github/workflows/grafana-ci-cd.yml` -- triggers on `grafana/**` changes
- `.github/workflows/sabnzbd-ci-cd.yml` -- triggers on `sabnzbd/**` changes
- `.github/workflows/argocd-ci-cd.yml` -- triggers on `argocd/**` changes

All use `mac-lucky/actions-shared-workflows/go-cicd-reusable.yml`. Triggers: push to `main`, tags (`v*`), pull requests to `main`, and manual `workflow_dispatch`. Docker images are built and pushed to GHCR (`ghcr.io/mac-lucky/pushward-{github,grafana,sabnzbd,argocd}`) on push to main or tags.

## Docker

All Dockerfiles use multi-stage builds: `golang:1.25-alpine` builder, `alpine:3.23` runtime. Binaries are statically compiled (`CGO_ENABLED=0`, stripped with `-ldflags="-s -w"`). Runtime runs as non-root `appuser` (UID 1000). Default config path in containers: `/config/config.yml`.

Grafana, SABnzbd, and ArgoCD Dockerfiles expose port 8090. The GitHub Dockerfile does not expose any ports (polling-only, no HTTP server).

## Configuration

All settings support YAML config file (`-config` flag, default `config.yml`) and environment variable overrides. Env vars always take precedence.

### Common env vars (all integrations)

| Variable | Description |
|---|---|
| `PUSHWARD_URL` | PushWard server URL |
| `PUSHWARD_API_KEY` | Integration key (`hlk_` prefix) |
| `PUSHWARD_PRIORITY` | Activity priority 0-10 (default: 1) |
| `PUSHWARD_CLEANUP_DELAY` | Delay before cleanup after ENDED (default: 15m) |

### GitHub-specific env vars

| Variable | Description |
|---|---|
| `PUSHWARD_GITHUB_TOKEN` | GitHub PAT with `actions:read` scope |
| `PUSHWARD_GITHUB_OWNER` | GitHub username for auto-discovery |
| `PUSHWARD_GITHUB_REPOS` | Comma-separated `owner/repo` list |
| `PUSHWARD_POLL_IDLE` | Idle poll interval (default: 60s) |
| `PUSHWARD_POLL_ACTIVE` | Active poll interval (default: 5s) |

### SABnzbd-specific env vars

| Variable | Description |
|---|---|
| `PUSHWARD_SABNZBD_URL` | SABnzbd API URL |
| `PUSHWARD_SABNZBD_API_KEY` | SABnzbd API key |
| `PUSHWARD_SERVER_ADDRESS` | HTTP listen address (default: `:8090`) |
| `PUSHWARD_POLL_INTERVAL` | Poll interval during tracking (default: 1s) |

### ArgoCD-specific env vars

| Variable | Description |
|---|---|
| `PUSHWARD_ARGOCD_WEBHOOK_SECRET` | Optional webhook secret for request validation |
| `PUSHWARD_SERVER_ADDRESS` | HTTP listen address (default: `:8090`) |
| `PUSHWARD_STALE_TIMEOUT` | Stale sync timeout (default: 30m) |
