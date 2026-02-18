# pushward-github

Bridges GitHub Actions CI/CD workflows to [PushWard](https://github.com/mac-lucky/pushward-server) Live Activities on iOS.

Polls the GitHub Actions API for in-progress workflow runs and sends real-time updates to PushWard, which displays them as Live Activities on your iPhone's Dynamic Island and Lock Screen.

## Prerequisites

- A running PushWard server
- A PushWard activity and integration key (`hlk_` prefix)
- A GitHub personal access token with `actions:read` scope (add `repo` for private repos)
- The PushWard iOS app subscribed to the activity

## Configuration

All settings can be provided via YAML config file or environment variables. Environment variables take precedence.

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_GITHUB_TOKEN` | `github.token` | GitHub PAT with `actions:read` | Yes |
| `PUSHWARD_GITHUB_OWNER` | `github.owner` | GitHub username — auto-discovers all repos | No* |
| `PUSHWARD_GITHUB_REPOS` | `github.repos` | Comma-separated `owner/repo` list | No* |
| `PUSHWARD_URL` | `pushward.url` | PushWard server URL | Yes |
| `PUSHWARD_API_KEY` | `pushward.api_key` | PushWard integration key (`hlk_`) | Yes |
| `PUSHWARD_POLL_IDLE` | `polling.idle_interval` | Poll interval when idle (default: 60s) | No |
| `PUSHWARD_POLL_ACTIVE` | `polling.active_interval` | Poll interval when active (default: 5s) | No |

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
2. **Idle**: Polls each repo every 60s for in-progress workflow runs
3. **Workflow found**: Creates a PushWard activity and sends `ONGOING` update (triggers push-to-start Live Activity on iOS)
4. **Active polling**: Every 5s, fetches jobs for the tracked run and updates progress
5. **Completed**: Sends `ENDED` update with success (green) or failure (red) status
6. **Cleanup**: After configurable delay (default 15m), deletes the activity
