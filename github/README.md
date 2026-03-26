# pushward-github

Bridges GitHub Actions CI/CD workflows to [PushWard](https://pushward.app) Live Activities on iOS.

Polls the GitHub Actions API for in-progress workflow runs and sends real-time updates to PushWard, which displays them as Live Activities on your iPhone's Dynamic Island and Lock Screen.

## Features

- **Repo auto-discovery** -- set `owner` and all non-archived, non-disabled repos are discovered automatically (refreshes every 5 min)
- **Matrix job grouping** -- parallel matrix jobs (e.g. `Build (ubuntu, node-16)`) are grouped into a single step with `step_rows` for multi-row progress display
- **Reusable workflow support** -- caller prefixes (`ci-cd / Build`) are stripped to show clean step names
- **URL links** -- each update includes the workflow run URL and a secondary repo link
- **Two-phase end** -- on completion, sends an `ONGOING` update with final content (after `end_delay`) so Dynamic Island shows the result, then sends `ENDED` (after `end_display_time`) to dismiss
- **Accent colors** -- green while running, red on failure
- **Rate limit handling** -- proactive backoff when remaining requests drop below 50, automatic retry on 429 responses with exponential backoff
- **Server-managed cleanup** -- activities are auto-deleted by the server after `ended_ttl` expires

## Prerequisites

- A running PushWard server
- A PushWard integration key (`hlk_` prefix) with `activity:manage` scope
- A GitHub personal access token with `actions:read` scope (add `repo` for private repos)
- The PushWard iOS app installed and subscribed

## Configuration

All settings can be provided via YAML config file or environment variables. Environment variables take precedence.

```yaml
github:
  token: ""           # or set PUSHWARD_GITHUB_TOKEN
  owner: "mac-lucky"  # or set PUSHWARD_GITHUB_OWNER — auto-discovers all repos
  repos:              # or set PUSHWARD_GITHUB_REPOS — optional when owner is set
    # - "other-org/some-repo"  # add repos outside owner if needed

pushward:
  url: ""             # or set PUSHWARD_URL (e.g. https://api.pushward.app)
  api_key: ""         # or set PUSHWARD_API_KEY (hlk_ integration key)
  # priority: 1            # PUSHWARD_PRIORITY (0-10)
  # cleanup_delay: 15m     # PUSHWARD_CLEANUP_DELAY
  # stale_timeout: 30m     # PUSHWARD_STALE_TIMEOUT
  # end_delay: 5s          # PUSHWARD_END_DELAY
  # end_display_time: 4s   # PUSHWARD_END_DISPLAY_TIME

polling:
  idle_interval: 60s  # or set PUSHWARD_POLL_IDLE
```

| Env Variable | Config Key | Description | Default |
|---|---|---|---|
| `PUSHWARD_GITHUB_TOKEN` | `github.token` | GitHub PAT with `actions:read` | *required* |
| `PUSHWARD_GITHUB_OWNER` | `github.owner` | GitHub username -- auto-discovers all repos | *required*\* |
| `PUSHWARD_GITHUB_REPOS` | `github.repos` | Comma-separated `owner/repo` list | *required*\* |
| `PUSHWARD_URL` | `pushward.url` | PushWard server URL | *required* |
| `PUSHWARD_API_KEY` | `pushward.api_key` | PushWard integration key (`hlk_`) | *required* |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Activity priority (0-10) | `1` |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | Passed as `ended_ttl` to server | `15m` |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Passed as `stale_ttl` to server | `30m` |
| `PUSHWARD_END_DELAY` | `pushward.end_delay` | Wait before sending final ONGOING update | `5s` |
| `PUSHWARD_END_DISPLAY_TIME` | `pushward.end_display_time` | Display time before sending ENDED | `4s` |
| `PUSHWARD_POLL_IDLE` | `polling.idle_interval` | Poll interval | `60s` |

\* At least one of `owner` or `repos` is required. When `owner` is set, all non-archived repos are discovered automatically. `repos` can be used alongside `owner` to add repos from other orgs.

## Build & Run

```bash
# Build from source
go build ./github/cmd/pushward-github

# Run
./pushward-github -config github/config.example.yml
```

## Docker

The Docker build context is the repo root (not the `github/` directory).

```bash
docker build -f github/Dockerfile -t pushward-github .
```

```yaml
# docker-compose.yml
services:
  pushward-github:
    image: ghcr.io/mac-lucky/pushward-github:latest
    volumes:
      - ./config.yml:/config/config.yml:ro
    environment:
      - PUSHWARD_GITHUB_TOKEN=ghp_xxxxxxxxxxxx
      - PUSHWARD_GITHUB_OWNER=mac-lucky
      - PUSHWARD_URL=https://api.pushward.app
      - PUSHWARD_API_KEY=hlk_xxxxxxxxxxxx
```

## How It Works

1. **Startup** -- if `owner` is set, discovers all non-archived repos via the GitHub API (refreshes every 5 min). Explicitly configured `repos` are merged in.
2. **Idle polling** -- polls each repo at the configured interval (default 60s) for in-progress workflow runs via `GET /repos/{owner}/{repo}/actions/runs?status=in_progress`.
3. **Workflow found** -- creates a PushWard activity with slug `gh-<repo-name>` and sends an initial `ONGOING` update using the `steps` template (triggers push-to-start Live Activity on iOS). Jobs are fetched immediately for accurate initial step counts.
4. **Active polling** -- on each cycle, fetches jobs for tracked runs, groups matrix jobs by base name into steps, and sends progress updates with step count, step rows, step labels, and accent color. The total step count only increases (never decreases) as GitHub lazily creates jobs behind `needs`/`if` conditions.
5. **Completed** -- schedules a two-phase end: first sends a final `ONGOING` update (green for success, red for failure/cancellation), then after `end_display_time` sends `ENDED` to dismiss the Live Activity. If a new workflow starts on the same repo while an end is pending, the end is cancelled and the new run takes over.
6. **Cleanup** -- the server auto-deletes the activity after the `ended_ttl` (cleanup_delay) expires.

## Tests

```bash
go test ./github/... -v -count=1
```
