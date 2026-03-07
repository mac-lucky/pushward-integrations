# pushward-docker

Collection of PushWard integration bridges packaged as Docker containers. Each bridge monitors an external service and sends real-time Live Activity updates to [pushward-server](https://github.com/mac-lucky/pushward-server) for display on iOS (Dynamic Island and Lock Screen).

## Integrations

| Integration | Description | Port | Docker Image |
|---|---|---|---|
| [pushward-github](./github/) | GitHub Actions CI/CD workflow progress | - | `ghcr.io/mac-lucky/pushward-github` |
| [pushward-grafana](./grafana/) | Grafana alert notifications | 8090 | `ghcr.io/mac-lucky/pushward-grafana` |
| [pushward-sabnzbd](./sabnzbd/) | SABnzbd download and post-processing progress | 8090 | `ghcr.io/mac-lucky/pushward-sabnzbd` |
| [pushward-argocd](./argocd/) | ArgoCD sync progress (Syncing -> Rolling Out -> Deployed) | 8090 | `ghcr.io/mac-lucky/pushward-argocd` |
| [pushward-bambulab](./bambulab/) | BambuLab 3D printer progress tracking via MQTT | - | `ghcr.io/mac-lucky/pushward-bambulab` |

## Common Configuration

All integrations require these PushWard connection settings (via environment variable or YAML config):

| Env Variable | Description |
|---|---|
| `PUSHWARD_URL` | PushWard server URL (e.g. `https://api.pushward.app`) |
| `PUSHWARD_API_KEY` | PushWard integration key (`hlk_` prefix) |

See each integration's README for the full list of configuration options.

## Project Structure

This is a Go workspace (`go.work`) with a shared module and five integration modules:

```
pushward-docker/
  go.work                  # Go workspace: ./shared, ./github, ./grafana, ./sabnzbd, ./argocd, ./bambulab
  shared/                  # Shared module used by all integrations
    config/                # Common PushWardConfig, ServerConfig types, YAML loader, env overrides
    pushward/              # PushWard API client (create/update/delete activities, retry, rate-limit)
    server/                # HTTP server boilerplate (health endpoint, graceful shutdown)
    testutil/              # Mock PushWard server, request recording, body unmarshalling
  github/                  # pushward-github integration
    cmd/pushward-github/   # Entry point
    internal/
      config/              # YAML + env var config loading
      github/              # GitHub Actions API client (workflow runs, jobs, repos)
      poller/              # Poll loop: idle/active intervals, tracked run state, cleanup
    Dockerfile
    config.example.yml
  grafana/                 # pushward-grafana integration
    cmd/pushward-grafana/  # Entry point (HTTP server + webhook handler)
    internal/
      config/              # YAML + env var config loading
      grafana/             # Grafana webhook payload types
      handler/             # Alert lifecycle: firing, resolved, stale timeout, cleanup
    Dockerfile
    config.example.yml
  sabnzbd/                 # pushward-sabnzbd integration
    cmd/pushward-sabnzbd/  # Entry point (HTTP server + tracker)
    internal/
      config/              # YAML + env var config loading
      sabnzbd/             # SABnzbd API client (queue, history)
      tracker/             # Download/PP tracking loop, webhook handler
    Dockerfile
    config.example.yml
  argocd/                  # pushward-argocd integration
    cmd/pushward-argocd/   # Entry point
    internal/
      config/              # YAML + env var config loading
      argocd/              # ArgoCD webhook payload types
      handler/             # Sync lifecycle: running, succeeded, deployed, failed
    Dockerfile
    config.example.yml
  bambulab/                # pushward-bambulab integration
    cmd/pushward-bambulab/ # Entry point
    internal/
      config/              # YAML + env var config loading
      bambulab/            # BambuLab MQTT client, printer state types
      tracker/             # Print lifecycle: preparing, printing, paused, finished, failed
    Dockerfile
    config.example.yml
  .github/workflows/       # Per-integration CI/CD pipelines
```

## Development

Build any integration from the repo root:

```bash
go build ./github/cmd/pushward-github
go build ./grafana/cmd/pushward-grafana
go build ./sabnzbd/cmd/pushward-sabnzbd
go build ./argocd/cmd/pushward-argocd
go build ./bambulab/cmd/pushward-bambulab
```

Run locally with a config file:

```bash
./pushward-github -config github/config.example.yml
./pushward-grafana -config grafana/config.example.yml
./pushward-sabnzbd -config sabnzbd/config.example.yml
./pushward-argocd -config argocd/config.example.yml
./pushward-bambulab -config bambulab/config.example.yml
```

Build Docker images:

```bash
docker build -f github/Dockerfile -t pushward-github .
docker build -f grafana/Dockerfile -t pushward-grafana .
docker build -f sabnzbd/Dockerfile -t pushward-sabnzbd .
docker build -f argocd/Dockerfile -t pushward-argocd .
docker build -f bambulab/Dockerfile -t pushward-bambulab .
```

## CI/CD

Each integration has its own GitHub Actions workflow with path filters so only the changed integration gets built:

- `.github/workflows/github-ci-cd.yml` -- triggers on `github/**` and `shared/**` changes
- `.github/workflows/grafana-ci-cd.yml` -- triggers on `grafana/**` and `shared/**` changes
- `.github/workflows/sabnzbd-ci-cd.yml` -- triggers on `sabnzbd/**` and `shared/**` changes
- `.github/workflows/argocd-ci-cd.yml` -- triggers on `argocd/**` and `shared/**` changes
- `.github/workflows/bambulab-ci-cd.yml` -- triggers on `bambulab/**` and `shared/**` changes

All use the shared `mac-lucky/actions-shared-workflows/go-cicd-reusable.yml` workflow. Triggers: push to `main`, tags (`v*`), pull requests to `main`, and manual `workflow_dispatch`. Docker images are built and pushed to GHCR on push to main or tags.
