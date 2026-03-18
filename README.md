# pushward-integrations

Collection of PushWard integration bridges packaged as Docker containers. Each bridge monitors an external service and sends real-time Live Activity updates to [pushward-server](https://pushward.app) for display on iOS (Dynamic Island and Lock Screen).

## Integrations

### Standalone Bridges

Each runs as its own container with a dedicated PushWard API key.

| Integration | Description | Port | Docker Image |
|---|---|---|---|
| [pushward-github](./github/) | GitHub Actions CI/CD workflow progress | - | `ghcr.io/mac-lucky/pushward-github` |
| [pushward-grafana](./grafana/) | Grafana alert notifications | 8090 | `ghcr.io/mac-lucky/pushward-grafana` |
| [pushward-sabnzbd](./sabnzbd/) | SABnzbd download and post-processing progress | 8090 | `ghcr.io/mac-lucky/pushward-sabnzbd` |
| [pushward-argocd](./argocd/) | ArgoCD sync progress (Syncing → Rolling Out → Deployed) | 8090 | `ghcr.io/mac-lucky/pushward-argocd` |
| [pushward-bambulab](./bambulab/) | BambuLab 3D printer progress via MQTT | - | `ghcr.io/mac-lucky/pushward-bambulab` |
| [pushward-mqtt](./mqtt/) | Generic MQTT-to-Live-Activity bridge | - | `ghcr.io/mac-lucky/pushward-mqtt` |

### Relay (Multi-Tenant Gateway)

[pushward-relay](./relay/) consolidates multiple providers into a single binary with PostgreSQL shared state. Each tenant authenticates with their own `hlk_` integration key per request.

| Provider | Route | Template | Description |
|---|---|---|---|
| Grafana | `POST /grafana` | alert | Alert firing/resolved lifecycle |
| ArgoCD | `POST /argocd` | pipeline | 3-step sync pipeline |
| Radarr | `POST /radarr` | generic | Movie grab/download/health/manual interaction |
| Sonarr | `POST /sonarr` | generic | TV episode grab/download/health/manual interaction |
| Jellyfin | `POST /jellyfin` | generic | Playback tracking, library additions, tasks, auth failures |
| Paperless-ngx | `POST /paperless` | generic | Document consumption and processing |
| Changedetection.io | `POST /changedetection` | alert | Page change notifications |
| Unmanic | `POST /unmanic` | generic | Transcoding task completion/failure |

All relay routes are wrapped with IP rate limiting, auth middleware (`hlk_` key extraction), and per-key rate limiting.

## Common Configuration

All integrations require these PushWard connection settings (via environment variable or YAML config):

| Env Variable | Description |
|---|---|
| `PUSHWARD_URL` | PushWard server URL (e.g. `https://api.pushward.app`) |
| `PUSHWARD_API_KEY` | PushWard integration key (`hlk_` prefix) — standalone bridges only |

The relay uses per-request `hlk_` keys instead of a single `PUSHWARD_API_KEY`.

See each integration's README or `config.example.yml` for the full list of configuration options.

## Project Structure

This is a Go workspace (`go.work`) with a shared module and eight integration modules:

```
pushward-integrations/
  go.work                    # Go workspace
  shared/                    # Shared module (API client, config, server, test utils)
  github/                    # GitHub Actions poller
  grafana/                   # Grafana webhook handler
  sabnzbd/                   # SABnzbd webhook + download tracker
  argocd/                    # ArgoCD webhook handler
  bambulab/                  # BambuLab MQTT client
  mqtt/                      # Generic MQTT bridge
  relay/                     # Multi-tenant webhook gateway (PostgreSQL)
    cmd/pushward-relay/      # Entry point
    internal/
      auth/                  # Auth middleware (hlk_ key extraction)
      client/                # LRU pool of per-tenant PushWard clients
      config/                # YAML config with per-provider settings
      grafana/               # Grafana alert handler
      argocd/                # ArgoCD sync handler
      starr/                 # Radarr/Sonarr handler (grab, download, health, manual interaction)
      jellyfin/              # Jellyfin playback, library, task, and auth handlers
      paperless/             # Paperless-ngx document consumption handler
      changedetection/       # Changedetection.io page change handler
      unmanic/               # Unmanic transcoding task handler
      ratelimit/             # IP and per-key rate limiting
      state/                 # PostgreSQL + in-memory state stores
    testdata/                # Fixture JSON files for all providers
    config.example.yml
    Dockerfile
  .github/workflows/         # Per-integration CI/CD pipelines
```

## Development

Build any integration from the repo root:

```bash
go build ./github/cmd/pushward-github
go build ./grafana/cmd/pushward-grafana
go build ./sabnzbd/cmd/pushward-sabnzbd
go build ./argocd/cmd/pushward-argocd
go build ./bambulab/cmd/pushward-bambulab
go build ./mqtt/cmd/pushward-mqtt
go build ./relay/cmd/pushward-relay
```

Run locally with a config file:

```bash
./pushward-github -config github/config.example.yml
./pushward-grafana -config grafana/config.example.yml
./pushward-sabnzbd -config sabnzbd/config.example.yml
./pushward-argocd -config argocd/config.example.yml
./pushward-bambulab -config bambulab/config.example.yml
./pushward-mqtt -config mqtt/config.example.yml
./pushward-relay -config relay/config.example.yml
```

Run tests:

```bash
# All tests
go test ./shared/... ./github/... ./grafana/... ./sabnzbd/... ./argocd/... ./bambulab/... ./mqtt/... ./relay/... -v -count=1

# Relay only (with race detector)
go test ./relay/... -race -count=1 -v
```

Build Docker images:

```bash
docker build -f github/Dockerfile -t pushward-github .
docker build -f grafana/Dockerfile -t pushward-grafana .
docker build -f sabnzbd/Dockerfile -t pushward-sabnzbd .
docker build -f argocd/Dockerfile -t pushward-argocd .
docker build -f bambulab/Dockerfile -t pushward-bambulab .
docker build -f mqtt/Dockerfile -t pushward-mqtt .
docker build -f relay/Dockerfile -t pushward-relay .
```

## CI/CD

Each integration has its own GitHub Actions workflow with path filters so only the changed integration gets built:

- `.github/workflows/github-ci-cd.yml` -- triggers on `github/**` and `shared/**` changes
- `.github/workflows/grafana-ci-cd.yml` -- triggers on `grafana/**` and `shared/**` changes
- `.github/workflows/sabnzbd-ci-cd.yml` -- triggers on `sabnzbd/**` and `shared/**` changes
- `.github/workflows/argocd-ci-cd.yml` -- triggers on `argocd/**` and `shared/**` changes
- `.github/workflows/bambulab-ci-cd.yml` -- triggers on `bambulab/**` and `shared/**` changes
- `.github/workflows/mqtt-ci-cd.yml` -- triggers on `mqtt/**` and `shared/**` changes
- `.github/workflows/relay-ci-cd.yml` -- triggers on `relay/**` and `shared/**` changes

All use the shared `mac-lucky/actions-shared-workflows/go-cicd-reusable.yml` workflow. Triggers: push to `main`, tags (`v*`), pull requests to `main`, and manual `workflow_dispatch`. Docker images are built and pushed to GHCR on push to main or tags.
