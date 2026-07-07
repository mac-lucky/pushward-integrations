[![Website](https://img.shields.io/badge/pushward.app-5B4FE5?style=for-the-badge&logo=safari&logoColor=white)](https://pushward.app)
[![App Store](https://img.shields.io/badge/App_Store-Download-0D96F6?style=for-the-badge&logo=apple&logoColor=white)](https://apps.apple.com/app/id6759689999)
[![CI/CD GitHub](https://github.com/mac-lucky/pushward-integrations/actions/workflows/github-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/github-ci-cd.yml)
[![Image](https://img.shields.io/badge/ghcr.io-pushward--github-2496ED?logo=docker&logoColor=white)](https://github.com/mac-lucky/pushward-integrations/pkgs/container/pushward-github)

# PushWard for GitHub Actions

Polls the GitHub Actions API for in-progress workflow runs and pushes their live progress to [PushWard](https://pushward.app) as iOS Live Activities on the Dynamic Island and Lock Screen.

`pushward-github` is a standalone, outbound-only poller: it reads the GitHub REST API and writes to the PushWard activities API. It runs no HTTP server of its own and serves a single PushWard account (one `hlk_` key per instance).

> **New to PushWard?** Learn more at **[pushward.app](https://pushward.app)** and get the iOS app on the **[App Store](https://apps.apple.com/app/id6759689999)**.

## How it works

```
GitHub Actions API ──poll──> pushward-github ──POST/PATCH /activities──> PushWard server ──APNs──> iOS Live Activity
```

1. **Discover** — if `github.owner` is set, every non-archived, non-disabled repo under that owner is listed and refreshed every 5 minutes; any `github.repos` you list are merged in and de-duplicated.
2. **Detect** — each repo is polled (default every 60s) for in-progress runs via `GET /repos/{owner}/{repo}/actions/runs?status=in_progress`; the most recently created run is tracked.
3. **Create** — a Live Activity is created (`POST /activities`) and seeded with the `steps` template, triggering a push-to-start Live Activity on subscribed iPhones.
4. **Update** — on each cycle the bridge fetches the run's jobs, groups matrix/reusable-workflow jobs by base name into steps, and sends `PATCH /activities/{slug}` only when something changed (or a heartbeat is due).
5. **End** — on completion it runs a two-phase end: a final `ONGOING` frame (green for success, red for failure/cancel) so the result lands on the Dynamic Island, then `ENDED` to dismiss the activity.

## Features

- **Repo auto-discovery** — set `github.owner` and all of that account's non-archived, non-disabled repos are monitored automatically, refreshed every 5 minutes.
- **Stable step total** — GitHub creates jobs lazily (behind `needs:`/`if:`), so a fresh scan can't know the final count. The `X/N` denominator is seeded from a prior finished run of the same workflow + branch (last success preferred), giving a steady total from the first frame; falls back to a live scan when there is no prior run.
- **Matrix & reusable-workflow grouping** — parallel matrix jobs (`Build (ubuntu, node-16)`) collapse into one step with per-shard `step_rows`; reusable caller prefixes (`ci-cd / Build` → `Build`) are stripped for clean labels.
- **Duration-sized, color-coded pills** — each step pill is sized (`step_weights`) by how long that group took in the previous run — the longest job for a matrix group — so a long build reads wider than a quick lint; pills fall back to equal widths when there is no prior run. Pills are also tinted (`step_colors`) by job type (tests, lint, build, docker, deploy, security).
- **Monotonic progress** — the total step count only ever clamps upward across polls; it never decreases mid-run.
- **Two-phase end** — a final result frame is held for `end_display_time` before the activity is dismissed; the last frame forces `N/N` so an over-counted seed self-heals to a full bar.
- **Accent colors & deep links** — green while running, red on failure; each update carries the workflow-run URL and a secondary link to the repository.
- **Eviction guards** — a tracked run is evicted if its jobs endpoint goes silent for longer than `stale_timeout + 30s`, and any run wedged `in_progress` is reclaimed after an absolute 12-hour lifetime so it never blocks new-run detection.
- **GitHub rate-limit handling** — proactively backs off when remaining requests drop to 50 or fewer, retries `429`/rate-limited `403` responses honoring `Retry-After` / `X-RateLimit-Reset`, and fails fast on other 4xx.
- **Server-managed cleanup** — `cleanup_delay` and `stale_timeout` are passed to the server as `ended_ttl` / `stale_ttl`, so finished and stalled activities are auto-deleted server-side.

## Prerequisites

- A running [PushWard](https://pushward.app) server (the public API is `https://api.pushward.app`).
- A PushWard integration key (`hlk_` prefix) with the activity-manage capability.
- A GitHub personal access token with `actions:read` (read workflow runs and jobs); add `repo` to discover and monitor private repositories.
- The PushWard iOS app installed and subscribed to the repos you want to see.

## Installation

The published image is on GHCR only (Docker Hub publishing is disabled in CI):

```bash
docker pull ghcr.io/mac-lucky/pushward-github:latest
```

### Docker run

```bash
docker run --rm \
  -e PUSHWARD_GITHUB_TOKEN=YOUR_GH_TOKEN \
  -e PUSHWARD_GITHUB_OWNER=your-github-username \
  -e PUSHWARD_URL=https://api.pushward.app \
  -e PUSHWARD_API_KEY=YOUR_API_KEY \
  ghcr.io/mac-lucky/pushward-github:latest
```

The official image pre-sets `PUSHWARD_URL=https://api.pushward.app`, so you can omit it when targeting the public server.

### Docker Compose

```yaml
services:
  pushward-github:
    image: ghcr.io/mac-lucky/pushward-github:latest
    restart: unless-stopped
    # Either mount a config file...
    volumes:
      - ./config.yml:/config/config.yml:ro
    # ...or configure entirely via env vars (these override the YAML):
    environment:
      - PUSHWARD_GITHUB_TOKEN=YOUR_GH_TOKEN
      - PUSHWARD_GITHUB_OWNER=your-github-username
      - PUSHWARD_URL=https://api.pushward.app
      - PUSHWARD_API_KEY=YOUR_API_KEY
```

The container runs as non-root (UID 1000); its entrypoint reads `-config /config/config.yml` by default.

## Configuration

Settings come from a YAML file and/or environment variables. **Environment variables (prefix `PUSHWARD_*`) override the YAML.** At least one of `github.owner` or `github.repos` is required; both can be combined.

```yaml
github:
  token: ""                        # or PUSHWARD_GITHUB_TOKEN
  owner: "your-github-username"    # or PUSHWARD_GITHUB_OWNER — auto-discovers all repos
  repos:                           # or PUSHWARD_GITHUB_REPOS (comma-separated) — optional when owner is set
    # - "other-org/some-repo"      # add repos outside owner if needed

pushward:
  url: ""                          # or PUSHWARD_URL (e.g. https://api.pushward.app)
  api_key: ""                      # or PUSHWARD_API_KEY (hlk_ integration key)
  # priority: 1                    # PUSHWARD_PRIORITY (0-10)
  # cleanup_delay: 15m             # PUSHWARD_CLEANUP_DELAY  -> server ended_ttl
  # stale_timeout: 30m             # PUSHWARD_STALE_TIMEOUT  -> server stale_ttl
  # end_delay: 5s                  # PUSHWARD_END_DELAY
  # end_display_time: 4s           # PUSHWARD_END_DISPLAY_TIME

polling:
  idle_interval: 60s               # or PUSHWARD_POLL_IDLE
```

| Env Variable | Config Key | Description | Required | Default |
|---|---|---|---|---|
| `PUSHWARD_GITHUB_TOKEN` | `github.token` | GitHub PAT (`actions:read`; add `repo` for private repos). Sent as `Authorization: Bearer`. | Yes | — |
| `PUSHWARD_GITHUB_OWNER` | `github.owner` | GitHub user/org login. When set, all non-archived, non-disabled repos are discovered and refreshed every 5 min. | One of owner/repos | — |
| `PUSHWARD_GITHUB_REPOS` | `github.repos` | Explicit `owner/repo` list (env: comma-separated), merged with discovered repos. | One of owner/repos | — |
| `PUSHWARD_URL` | `pushward.url` | PushWard server base URL. Required in config, but the official image pre-sets `https://api.pushward.app`. | Yes¹ | — (image: `https://api.pushward.app`) |
| `PUSHWARD_API_KEY` | `pushward.api_key` | PushWard integration key (`hlk_` prefix). | Yes | — |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Activity priority sent to the server (validated 0–10). | No | `1` |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | Passed as `ended_ttl`: how long the server keeps an activity after it ends. | No | `15m` |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Passed as `stale_ttl`; also drives the heartbeat interval (`/2`) and the stale-run eviction guard (`+30s`). | No | `30m` |
| `PUSHWARD_END_DELAY` | `pushward.end_delay` | Wait after run completion before the final `ONGOING` frame (two-phase end, phase 1). | No | `5s` |
| `PUSHWARD_END_DISPLAY_TIME` | `pushward.end_display_time` | How long the final frame shows before `ENDED` dismisses the activity (phase 2). | No | `4s` |
| `PUSHWARD_POLL_IDLE` | `polling.idle_interval` | Interval between polling cycles for in-progress runs and active job updates. | No | `60s` |

¹ Required at the config layer; effectively optional when running the official image, which sets `PUSHWARD_URL` to the public API.

> Note: the comment in `config.example.yml` lists `stale_timeout: 60m`, but the in-code default is `30m` (shown above). Set it explicitly if you depend on a specific value.

## How it maps to a Live Activity

Each tracked run becomes one PushWard activity:

- **Slug** — `gh-<8 hex chars>`, derived from `SHA-256(owner/repo)` (e.g. `gh-1a2b3c4d`), stable per repository across runs.
- **Display name** — `GitHub: <repo-name>`.
- **Template** — `steps`, with `progress`, `current_step`/`total_steps`, `step_rows`, `step_labels`, `step_colors`, and `step_weights`.
- **Accent color** — green while running, red on failure/cancel.
- **Links** — primary URL is the workflow run's `html_url`; secondary URL is `https://github.com/<owner>/<repo>`.

## Development

This bridge is part of the `pushward-integrations` Go workspace (`go.work`), with a `replace` directive pointing `shared` at `../shared`. `go.mod` requires Go `1.26.4`.

```bash
# Build from source (from the pushward-integrations workspace root)
go build ./github/cmd/pushward-github

# Or from inside this directory (github/)
go build ./cmd/pushward-github

# Run with a config file
./pushward-github -config github/config.example.yml

# Tests (CI runs: -race -count=1 -v)
go test ./github/... -race -count=1 -v

# Lint (matches CI)
golangci-lint run
```

### Docker build

The build context is the **repository root** (so the Dockerfile can `COPY shared/`), not the `github/` directory:

```bash
docker build -f github/Dockerfile -t pushward-github .

# Optionally pin the build-time Go version (Dockerfile default: 1.26.4)
docker build -f github/Dockerfile --build-arg GO_VERSION=1.26.4 -t pushward-github .
```

## CI/CD & Releases

Bridges are versioned independently. The per-bridge workflow `.github/workflows/github-ci-cd.yml` runs on changes to `github/**` or `shared/**` and calls the shared `go-cicd-reusable.yml`. Images publish to **GHCR only** (`push_to_dockerhub: false`).

| Trigger | GHCR tags published |
|---|---|
| Pull request | _(none — tests + analysis only)_ |
| Push to `main` | `:main`, `:main-<short-sha>` |
| Git tag `github/v<X.Y.Z>` | `:X.Y.Z`, `:X.Y`, `:latest` (and `:X` once `X >= 1`) |

`:latest` moves only on tagged releases — never on a `main` push.

```bash
# Cut a release
git tag github/v0.4.1
git push origin github/v0.4.1
```

The release pipeline (`.github/workflows/release.yml`) produces a per-bridge GitHub Release with an auto-generated changelog (`.github/release.yml`).

## Server compatibility

This bridge targets the [pushward-server](https://pushward.app) REST surface — `POST /activities` (create) and `PATCH /activities/{slug}` (seed / update / end) — via the hand-written shared `pushward.Client`. The contract (routes, JSON keys, auth headers) is owned by pushward-server's `openapi.yaml`; the bridge tracks it at `MAJOR.MINOR`, and patch releases are bridge-only fixes that need no coordinated server bump. Released iOS clients can't be hot-fixed, so the activity slug, template, and `ContentState` shape are part of the contract.

## Troubleshooting

Logs are structured JSON written to stdout at info level. View them with `docker logs <container>` (or `docker compose logs -f pushward-github`); the startup line echoes the configured `owner`, `repos`, and `priority`.

| Symptom | Likely cause / fix |
|---|---|
| `github.token is required` on startup | Set `PUSHWARD_GITHUB_TOKEN` (or `github.token`). |
| `github.repos or github.owner is required` | Set at least one of `PUSHWARD_GITHUB_OWNER` / `PUSHWARD_GITHUB_REPOS`. |
| `failed to create activity` / auth errors to PushWard | Wrong or missing `PUSHWARD_API_KEY` (must be a valid `hlk_` key with activity-manage), or wrong `PUSHWARD_URL`. |
| GitHub `401`/`403`, or private repos not discovered | Token lacks `actions:read`, or lacks `repo` for private repositories. |
| Logs show rate-limit backoff / waits | The token's GitHub rate budget is low; the bridge waits and retries automatically. Use a dedicated token if it is shared with heavy usage. |
| No Live Activity appears on the phone | No in-progress run yet, the iOS app isn't subscribed to that repo's slug, or no compatible iOS build is installed. |

## License

`pushward-github` is published as part of the public [pushward-integrations](https://github.com/mac-lucky/pushward-integrations) repository. See the repository root for licensing.
