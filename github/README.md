# pushward-github

Bridges GitHub Actions CI/CD workflows to [PushWard](https://github.com/mac-lucky/pushward-server) Live Activities on iOS.

Polls the GitHub Actions API for in-progress workflow runs and sends real-time updates to PushWard, which displays them as Live Activities on your iPhone's Dynamic Island and Lock Screen.

## Features

- **Repo auto-discovery** -- set `owner` and all non-archived repos are discovered automatically (refreshes every 5 min)
- **Matrix job grouping** -- parallel matrix jobs (e.g. `Build (ubuntu, node-16)`) are grouped into a single pipeline step with `step_rows` for multi-row progress display
- **URL links** -- each update includes the workflow run URL and a secondary repo link (`https://github.com/<owner>/<repo>`)
- **Two-phase end** -- on completion, sends an `ONGOING` update with final content (after `end_delay`) so Dynamic Island shows the result, then sends `ENDED` (after `end_display_time`) to dismiss
- **Accent colors** -- green while running, red on failure
- **Server-managed cleanup** -- activities are auto-deleted by the server after `ended_ttl` expires (default 15m)

## Prerequisites

- A running PushWard server
- A PushWard integration key (`hlk_` prefix) with `activity:manage` scope
- A GitHub personal access token with `actions:read` scope (add `repo` for private repos)
- The PushWard iOS app installed and subscribed

## Configuration

All settings can be provided via YAML config file or environment variables. Environment variables take precedence.

| Env Variable | Config Key | Description | Default |
|---|---|---|---|
| `PUSHWARD_GITHUB_TOKEN` | `github.token` | GitHub PAT with `actions:read` | *required* |
| `PUSHWARD_GITHUB_OWNER` | `github.owner` | GitHub username — auto-discovers all repos | *required** |
| `PUSHWARD_GITHUB_REPOS` | `github.repos` | Comma-separated `owner/repo` list | *required** |
| `PUSHWARD_URL` | `pushward.url` | PushWard server URL | *required* |
| `PUSHWARD_API_KEY` | `pushward.api_key` | PushWard integration key (`hlk_`) | *required* |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Activity priority 0-10 | `1` |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | Passed as `ended_ttl` to server | `15m` |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Passed as `stale_ttl` to server | `30m` |
| `PUSHWARD_END_DELAY` | `pushward.end_delay` | Wait before sending final ONGOING update | `5s` |
| `PUSHWARD_END_DISPLAY_TIME` | `pushward.end_display_time` | Display time before sending ENDED | `4s` |
| `PUSHWARD_POLL_IDLE` | `polling.idle_interval` | Poll interval when idle | `60s` |

\* At least one of `PUSHWARD_GITHUB_OWNER` or `PUSHWARD_GITHUB_REPOS` is required. When `owner` is set, all non-archived repos are discovered automatically. `repos` can be used alongside `owner` to add repos from other orgs.

## Docker Compose

```yaml
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

1. **Startup**: If `owner` is set, discovers all repos via GitHub API (refreshes every 5 min)
2. **Idle polling**: Polls each repo every 60s for in-progress workflow runs
3. **Workflow found**: Creates a PushWard activity (`gh-<repo-name>` slug) and sends an initial `ONGOING` update with the `pipeline` template (triggers push-to-start Live Activity on iOS)
4. **Active polling**: On each idle cycle, fetches jobs for tracked runs, groups matrix jobs into pipeline steps, and updates progress with step count, step rows, and accent color
5. **Completed**: Schedules a two-phase end -- sends a final `ONGOING` update (success in green, failure in red) then `ENDED` to dismiss the Live Activity
6. **Cleanup**: The server auto-deletes the activity after `ended_ttl` expires (default 15m)
