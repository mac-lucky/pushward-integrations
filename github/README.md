# pushward-github

Bridges GitHub Actions CI/CD workflows to [PushWard](https://github.com/mac-lucky/pushward-server) Live Activities on iOS.

Polls the GitHub Actions API for in-progress workflow runs and sends real-time updates to PushWard, which displays them as Live Activities on your iPhone's Dynamic Island and Lock Screen.

## Prerequisites

- A running PushWard server
- A PushWard activity and integration key (`hlk_` prefix)
- A GitHub personal access token with `actions:read` scope
- The PushWard iOS app subscribed to the activity

## Configuration

All settings can be provided via YAML config file or environment variables. Environment variables take precedence.

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_GITHUB_TOKEN` | `github.token` | GitHub PAT with `actions:read` | Yes |
| `PUSHWARD_GITHUB_REPOS` | `github.repos` | Comma-separated `owner/repo` list | Yes |
| `PUSHWARD_URL` | `pushward.url` | PushWard server URL | Yes |
| `PUSHWARD_API_KEY` | `pushward.api_key` | PushWard integration key (`hlk_`) | Yes |
| `PUSHWARD_ACTIVITY_SLUG` | `pushward.activity_slug` | Activity slug to update | Yes |
| `PUSHWARD_POLL_IDLE` | `polling.idle_interval` | Poll interval when idle (default: 60s) | No |
| `PUSHWARD_POLL_ACTIVE` | `polling.active_interval` | Poll interval when active (default: 5s) | No |

## Docker Compose

```yaml
services:
  pushward-github:
    image: ghcr.io/mac-lucky/pushward-github:latest
    volumes:
      - ./config.yml:/config/config.yml:ro
    environment:
      - PUSHWARD_GITHUB_TOKEN=ghp_xxxxxxxxxxxx
      - PUSHWARD_URL=https://pushward.macluckylab.com
      - PUSHWARD_API_KEY=hlk_xxxxxxxxxxxx
      - PUSHWARD_ACTIVITY_SLUG=github-ci
      - PUSHWARD_GITHUB_REPOS=mac-lucky/pushward-server
```

## How It Works

1. **Idle**: Polls each configured repo every 60s for in-progress workflow runs
2. **Workflow found**: Sends `ONGOING` update (triggers push-to-start Live Activity on iOS)
3. **Active polling**: Every 5s, fetches jobs for the tracked run and updates progress
4. **Completed**: Sends `ENDED` update with success (green) or failure (red) status
5. **Back to idle**: Resumes polling for new workflows
