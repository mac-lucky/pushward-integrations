[![Website](https://img.shields.io/badge/pushward.app-5B4FE5?style=for-the-badge&logo=safari&logoColor=white)](https://pushward.app)
[![TestFlight](https://img.shields.io/badge/TestFlight-Join_Beta-0D96F6?style=for-the-badge&logo=apple&logoColor=white)](https://testflight.apple.com/join/T4aT6s3W)

# pushward-integrations

[![CI/CD GitHub](https://github.com/mac-lucky/pushward-integrations/actions/workflows/github-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/github-ci-cd.yml)
[![CI/CD SABnzbd](https://github.com/mac-lucky/pushward-integrations/actions/workflows/sabnzbd-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/sabnzbd-ci-cd.yml)
[![CI/CD BambuLab](https://github.com/mac-lucky/pushward-integrations/actions/workflows/bambulab-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/bambulab-ci-cd.yml)
[![CI/CD Relay](https://github.com/mac-lucky/pushward-integrations/actions/workflows/relay-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/relay-ci-cd.yml)
[![CI/CD Grafana](https://github.com/mac-lucky/pushward-integrations/actions/workflows/grafana-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/grafana-ci-cd.yml)

Collection of PushWard integration bridges packaged as Docker containers. Each bridge monitors an external service and sends real-time Live Activity updates to [pushward-server](https://pushward.app) for display on iOS (Dynamic Island and Lock Screen).

> **New to PushWard?** Learn more at **[pushward.app](https://pushward.app)** and join the iOS beta on **[TestFlight](https://testflight.apple.com/join/T4aT6s3W)**.

## Integrations

### Standalone Bridges

Each runs as its own container with a dedicated PushWard API key.

| Integration | Description | Port | Docker Image |
|---|---|---|---|
| [pushward-github](./github/) | GitHub Actions CI/CD workflow progress | - | `ghcr.io/mac-lucky/pushward-github` |
| [pushward-sabnzbd](./sabnzbd/) | SABnzbd download and post-processing progress | 8090 | `ghcr.io/mac-lucky/pushward-sabnzbd` |
| [pushward-bambulab](./bambulab/) | BambuLab 3D printer progress via MQTT | - | `ghcr.io/mac-lucky/pushward-bambulab` |
| [pushward-grafana](./grafana/) | Grafana alert timeline sparklines with Prometheus history backfill and multi-instance tracking | 8090 | `ghcr.io/mac-lucky/pushward-grafana` |

### Relay (Multi-Tenant Gateway)

[pushward-relay](./relay/) consolidates multiple providers into a single binary with PostgreSQL shared state. Each tenant authenticates with their own `hlk_` integration key per request.

| Provider | Route | Template | Description |
|---|---|---|---|
| Grafana | `POST /grafana` | alert | Alert firing/resolved lifecycle |
| ArgoCD | `POST /argocd` | steps | 3-step sync pipeline |
| Radarr | `POST /radarr` | generic | Movie grab/download/health/manual interaction |
| Sonarr | `POST /sonarr` | generic | TV episode grab/download/health/manual interaction |
| Jellyfin | `POST /jellyfin` | generic | Playback tracking, library additions, tasks, auth failures |
| Paperless-ngx | `POST /paperless` | generic | Document consumption and processing |
| Changedetection.io | `POST /changedetection` | alert | Page change notifications |
| Unmanic | `POST /unmanic` | generic | Transcoding task completion/failure |
| Proxmox VE | `POST /proxmox` | steps/alert | Backup, replication, fencing, and package update notifications |
| Overseerr/Jellyseerr | `POST /overseerr` | steps | Media request lifecycle (pending → approved → available) |
| Uptime Kuma | `POST /uptimekuma` | alert | Monitor up/down status changes |
| Gatus | `POST /gatus` | alert | Endpoint health status changes |
| Backrest | `POST /backrest` | generic | Backup operation progress and completion |

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

This is a Go workspace (`go.work`) with a shared module and five integration modules:

```
pushward-integrations/
  go.work                    # Go workspace
  shared/                    # Shared module (API client, config, server, text, test utils)
  github/                    # GitHub Actions poller
  sabnzbd/                   # SABnzbd webhook + download tracker
  bambulab/                  # BambuLab MQTT client
  grafana/                   # Grafana alert timeline with Prometheus history
    cmd/pushward-grafana/    # Entry point
    internal/
      config/                # YAML config with env overrides
      grafana/               # Grafana API client (auto-extract, alert state checks)
      handler/               # Webhook handler (multi-instance fingerprint tracking)
      metrics/               # Prometheus/VictoriaMetrics query client
      poller/                # Per-alert polling goroutines
    config.example.yml
    Dockerfile
  relay/                     # Multi-tenant webhook gateway (PostgreSQL)
    cmd/pushward-relay/      # Entry point
    internal/
      auth/                  # Auth middleware (hlk_ key extraction)
      client/                # LRU pool of per-tenant PushWard clients
      config/                # YAML config with per-provider settings
      lifecycle/             # Shared two-phase end logic
      lrumap/                # Generic LRU map for dedup/state
      ratelimit/             # IP and per-key rate limiting
      selftest/              # Webhook self-test support
      state/                 # PostgreSQL + in-memory state stores
      grafana/               # Grafana alert handler
      argocd/                # ArgoCD sync handler
      starr/                 # Radarr/Sonarr handler (grab, download, health, manual interaction)
      jellyfin/              # Jellyfin playback, library, task, and auth handlers
      paperless/             # Paperless-ngx document consumption handler
      changedetection/       # Changedetection.io page change handler
      unmanic/               # Unmanic transcoding task handler
      proxmox/               # Proxmox VE backup, replication, fencing, and update handler
      overseerr/             # Overseerr/Jellyseerr media request handler
      uptimekuma/            # Uptime Kuma monitor status handler
      gatus/                 # Gatus endpoint health handler
      backrest/              # Backrest backup operation handler
    testdata/                # Fixture JSON files for all providers
    config.example.yml
    Dockerfile
  .github/workflows/         # Per-integration CI/CD pipelines
```

## Development

Build any integration from the repo root:

```bash
go build ./github/cmd/pushward-github
go build ./sabnzbd/cmd/pushward-sabnzbd
go build ./bambulab/cmd/pushward-bambulab
go build ./relay/cmd/pushward-relay
go build ./grafana/cmd/pushward-grafana
```

Run locally with a config file:

```bash
./pushward-github -config github/config.example.yml
./pushward-sabnzbd -config sabnzbd/config.example.yml
./pushward-bambulab -config bambulab/config.example.yml
./pushward-relay -config relay/config.example.yml
./pushward-grafana -config grafana/config.example.yml
```

Run tests:

```bash
# All tests
go test ./shared/... ./github/... ./sabnzbd/... ./bambulab/... ./relay/... ./grafana/... -v -count=1

# Relay only (with race detector)
go test ./relay/... -race -count=1 -v
```

Build Docker images:

```bash
docker build -f github/Dockerfile -t pushward-github .
docker build -f sabnzbd/Dockerfile -t pushward-sabnzbd .
docker build -f bambulab/Dockerfile -t pushward-bambulab .
docker build -f relay/Dockerfile -t pushward-relay .
docker build -f grafana/Dockerfile -t pushward-grafana .
```

## CI/CD

Each bridge has its own per-bridge workflow plus a shared release orchestrator. All call the reusable `mac-lucky/actions-shared-workflows/go-cicd-reusable.yml` under the hood.

### Per-bridge CI workflows (PR + main)

Path-filtered so only the changed bridge runs:

- `.github/workflows/github-ci-cd.yml` — triggers on `github/**` and `shared/**`
- `.github/workflows/sabnzbd-ci-cd.yml` — triggers on `sabnzbd/**` and `shared/**`
- `.github/workflows/bambulab-ci-cd.yml` — triggers on `bambulab/**` and `shared/**`
- `.github/workflows/relay-ci-cd.yml` — triggers on `relay/**` and `shared/**`
- `.github/workflows/grafana-ci-cd.yml` — triggers on `grafana/**` and `shared/**`

### Release orchestrator (tags)

`.github/workflows/release.yml` listens for per-bridge git tags and runs the release pipeline for that one bridge.

### Image tag channels

| Trigger | GHCR tags published | Purpose |
|---|---|---|
| Pull request | _(none)_ | Tests + analysis only |
| Push to `main` | `:main`, `:main-<short-sha>` | Rolling latest unstable + immutable per-commit pin |
| Git tag `<bridge>/v<X.Y.Z>` | `:X.Y.Z`, `:X.Y`, `:latest` (and `:X` once `X >= 1`) | Stable release |

`:latest` is updated only on tag releases — it never moves on a `main` push.

### Cutting a release

Bridges are versioned independently with per-bridge git tags. Tag format: `<bridge>/v<X.Y.Z>`.

```bash
# Single-bridge release (typical bug-fix path)
git tag relay/v0.4.1
git push origin relay/v0.4.1

# Coordinated platform release (initial baseline / breaking API change)
for b in bambulab github grafana relay sabnzbd; do
  git tag "$b/v0.4.0"
done
git push origin bambulab/v0.4.0 github/v0.4.0 grafana/v0.4.0 relay/v0.4.0 sabnzbd/v0.4.0
```

Each tag triggers `release.yml` independently and produces a per-bridge GitHub Release with auto-generated changelog notes (categorized via `.github/release.yml`).

### Server compatibility

Bridges target the [pushward-server](https://pushward.app) API surface in their `MAJOR.MINOR`. Patch versions (`*.*.X`) are bridge-only fixes that do not require a coordinated server bump. When the server protocol changes, bump the affected bridges' minor.
