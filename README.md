# pushward-docker

Collection of PushWard integration bridges packaged as Docker containers. Each bridge monitors an external service and sends real-time Live Activity updates to [pushward-server](https://github.com/mac-lucky/pushward-server) for display on iOS (Dynamic Island and Lock Screen).

## Integrations

| Integration | Description | Port | Docker Image |
|---|---|---|---|
| [pushward-github](./github/) | GitHub Actions CI/CD workflow progress | - | `ghcr.io/mac-lucky/pushward-github` |
| [pushward-sabnzbd](./sabnzbd/) | SABnzbd download and post-processing progress | 8090 | `ghcr.io/mac-lucky/pushward-sabnzbd` |

## Common Configuration

All integrations require these PushWard connection settings (via environment variable or YAML config):

| Env Variable | Description |
|---|---|
| `PUSHWARD_URL` | PushWard server URL (e.g. `https://pushward.macluckylab.com`) |
| `PUSHWARD_API_KEY` | PushWard integration key (`hlk_` prefix) |

See each integration's README for the full list of configuration options.

## Project Structure

This is a Go workspace (`go.work`) with two independent modules:

```
pushward-docker/
  go.work                  # Go workspace: ./github, ./sabnzbd
  github/                  # pushward-github integration
    cmd/pushward-github/   # Entry point
    internal/
      config/              # YAML + env var config loading
      github/              # GitHub Actions API client (workflow runs, jobs, repos)
      poller/              # Poll loop: idle/active intervals, tracked run state, cleanup
      pushward/            # PushWard API client (create/update/delete activities)
    Dockerfile
    config.example.yml
  sabnzbd/                 # pushward-sabnzbd integration
    cmd/pushward-sabnzbd/  # Entry point (HTTP server + tracker)
    internal/
      config/              # YAML + env var config loading
      sabnzbd/             # SABnzbd API client (queue, history)
      tracker/             # Download/PP tracking loop, webhook handler
      pushward/            # PushWard API client (create/update/delete activities)
    Dockerfile
    config.example.yml
  .github/workflows/       # Per-integration CI/CD pipelines
```

## Development

Build any integration from the repo root:

```bash
go build ./github/cmd/pushward-github
go build ./sabnzbd/cmd/pushward-sabnzbd
```

Run locally with a config file:

```bash
./pushward-github -config github/config.example.yml
./pushward-sabnzbd -config sabnzbd/config.example.yml
```

## CI/CD

Each integration has its own GitHub Actions workflow with path filters so only the changed integration gets built:

- `.github/workflows/github-ci-cd.yml` -- triggers on `github/**` changes
- `.github/workflows/sabnzbd-ci-cd.yml` -- triggers on `sabnzbd/**` changes

Both use the shared `mac-lucky/actions-shared-workflows/go-cicd-reusable.yml` workflow. On push to main or tags (`v*`), Docker images are built and pushed to GHCR.
