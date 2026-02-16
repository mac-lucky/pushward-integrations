# pushward-docker

Collection of PushWard integration bridges as Docker containers. Each integration monitors an external service and sends Live Activity updates to [PushWard](https://github.com/mac-lucky/pushward-server) for display on iOS.

## Integrations

| Integration | Description | Port |
|---|---|---|
| [pushward-github](./github/) | Bridges GitHub Actions CI/CD workflows to Live Activities | - |
| [pushward-sabnzbd](./sabnzbd/) | Bridges SABnzbd download progress to Live Activities | 8090 |

## Common Configuration

All integrations share the same PushWard connection settings:

| Env Variable | Description |
|---|---|
| `PUSHWARD_URL` | PushWard server URL |
| `PUSHWARD_API_KEY` | PushWard integration key (`hlk_` prefix) |

See each integration's README for full configuration details.

## Development

This is a Go workspace. Build any integration from the repo root:

```bash
go build ./github/cmd/pushward-github
go build ./sabnzbd/cmd/pushward-sabnzbd
```
