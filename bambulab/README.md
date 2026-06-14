[![Website](https://img.shields.io/badge/pushward.app-5B4FE5?style=for-the-badge&logo=safari&logoColor=white)](https://pushward.app)
[![App Store](https://img.shields.io/badge/App_Store-Download-0D96F6?style=for-the-badge&logo=apple&logoColor=white)](https://apps.apple.com/app/id6759689999)
[![CI/CD BambuLab](https://github.com/mac-lucky/pushward-integrations/actions/workflows/bambulab-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/bambulab-ci-cd.yml)
[![Image](https://img.shields.io/badge/ghcr.io-pushward--bambulab-2496ED?logo=docker&logoColor=white)](https://github.com/mac-lucky/pushward-integrations/pkgs/container/pushward-bambulab)

# PushWard for Bambu Lab

Mirrors a Bambu Lab 3D printer's print progress as a [PushWard](https://pushward.app) Live Activity on iOS — progress bar, layer count, remaining time, and nozzle temperature live on the Dynamic Island and Lock Screen. Connects directly to the printer over local MQTT (no Bambu cloud), and reports paused, finished, failed, and cancelled prints in real time.

> **New to PushWard?** Learn more at **[pushward.app](https://pushward.app)** and get the iOS app on the **[App Store](https://apps.apple.com/app/id6759689999)**.

## How it works

```
Printer (MQTT/TLS :8883) --> pushward-bambulab --> pushward-server (REST) --> APNs --> iOS Live Activity
```

The bridge connects to the printer over TLS MQTT on the local network, subscribes to `device/<serial>/report`, and forces a full state snapshot on every connect (so delta-only P1/A1 printers don't hold stale state). It maps the printer's `gcode_state` to a PushWard **Live Activity** (`generic` template) via the server REST API — `POST /activities` to create, `PATCH /activities/{slug}` to update and end. The server pushes APNs updates to the [pushward-ios](https://pushward.app) app. The bridge exposes **no HTTP server or ports** — it is an outbound-only client.

One Live Activity is maintained per printer, keyed by the constant slug `bambu-<serial>` (serial lowercased), and reused across prints.

## Features

- **Real-time print tracking** — progress bar, `Layer N/M` (or `N%` when total layers are unknown), remaining-time countdown, filename, and nozzle temperature.
- **Full print lifecycle** — preparing, printing, paused, finished, failed, cancelled, and interrupted states, each with its own icon and accent color.
- **Local MQTT, no cloud** — connects directly to the printer over LAN; the Bambu cloud is never involved.
- **Delta-state merging** — pointer-based merge keeps unsent fields from prior pushes, correctly handling P1/A1 delta-only reports; a `pushall` is re-sent on every (re)connect.
- **TLS cert pinning** — pin the printer's self-signed cert by SHA-256 fingerprint, or rely on trust-on-first-use auto-pinning (default); insecure-skip-verify is opt-in only.
- **Resilient startup** — retries the initial connect every 30s until the printer powers on, then relies on MQTT auto-reconnect.
- **Auto-resume** — detects an in-progress print on startup (or once the first report arrives) and resumes tracking.
- **Two-phase end** — shows the completion/failure frame on the Dynamic Island before dismissing the activity.
- **Retry with backoff** — PushWard API calls retry up to 5 times with exponential backoff + jitter and honor `Retry-After` on 429.
- **Graceful shutdown** — ends the active activity as `Interrupted` on `SIGINT`/`SIGTERM`, then disconnects MQTT.

## Prerequisites

- A running **PushWard server** (public production base: `https://api.pushward.app`).
- A PushWard **integration key** (`hlk_` prefix) with the `activity:manage` scope.
- The **PushWard iOS app** (from the [App Store](https://apps.apple.com/app/id6759689999)), subscribed to the activity.
- A **Bambu Lab printer** with **Developer Mode / LAN Mode** enabled, reachable on your local network. You will need:
  - The printer's local **IP address** (or hostname).
  - The **Access Code** (printer Settings → WLAN) — used as the MQTT password.
  - The printer's **serial number** — used in MQTT topics, the MQTT client ID, and the activity slug.

> The access code (typically 8 characters) and serial (typically 15 characters) are Bambu hardware values. The bridge only checks they are non-empty — it does not validate their length or format.

## Installation

Pull the published image (GHCR only):

```bash
docker pull ghcr.io/mac-lucky/pushward-bambulab:latest
```

### Docker

```bash
docker run -d --name pushward-bambulab \
  -e PUSHWARD_URL=https://api.pushward.app \
  -e PUSHWARD_API_KEY=YOUR_API_KEY \
  -e PUSHWARD_BAMBULAB_HOST=<printer-ip> \
  -e PUSHWARD_BAMBULAB_ACCESS_CODE=<access-code> \
  -e PUSHWARD_BAMBULAB_SERIAL=<serial> \
  ghcr.io/mac-lucky/pushward-bambulab:latest
```

### Docker Compose

```yaml
services:
  pushward-bambulab:
    image: ghcr.io/mac-lucky/pushward-bambulab:latest
    restart: unless-stopped
    environment:
      - PUSHWARD_URL=https://api.pushward.app
      - PUSHWARD_API_KEY=YOUR_API_KEY
      - PUSHWARD_BAMBULAB_HOST=<printer-ip>
      - PUSHWARD_BAMBULAB_ACCESS_CODE=<access-code>
      - PUSHWARD_BAMBULAB_SERIAL=<serial>
```

The container runs as non-root (uid 1000) and reads its config from `/config/config.yml` by default (the `CMD` is `-config /config/config.yml`). Env vars alone are enough; to use a YAML file instead, mount it:

```yaml
    volumes:
      - ./config.yml:/config/config.yml:ro
```

## Configuration

All settings come from a YAML config file and/or environment variables. **Environment variables override YAML.** The standardized env prefix is `PUSHWARD_*`. See [`config.example.yml`](./config.example.yml) for the canonical file.

### Printer

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_BAMBULAB_HOST` | `bambulab.host` | Printer IP address or hostname on the local network. | Yes |
| `PUSHWARD_BAMBULAB_ACCESS_CODE` | `bambulab.access_code` | Printer LAN access code (Settings → WLAN); used as the MQTT password (username `bblp`). | Yes |
| `PUSHWARD_BAMBULAB_SERIAL` | `bambulab.serial` | Printer serial number; used in MQTT topics, client ID, and the activity slug `bambu-<serial>`. | Yes |
| `PUSHWARD_BAMBULAB_CERT_FINGERPRINT` | `bambulab.tls.cert_fingerprint_sha256` | SHA-256 fingerprint of the printer's cert to pin (hex, optional `:` separators). See [TLS verification](#tls-verification). | No (default: empty → auto-pin) |
| _(no env override)_ | `bambulab.tls.insecure_skip_verify` | When `true`, accept any printer TLS cert (logs a startup warning). Only consulted when no fingerprint is set. | No (default: `false`) |

### PushWard

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_URL` | `pushward.url` | PushWard server base URL — `https://api.pushward.app`. | Yes |
| `PUSHWARD_API_KEY` | `pushward.api_key` | Integration key (`hlk_`) with `activity:manage` scope; sent as `Authorization: Bearer`. | Yes |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Live Activity priority; must be `0`–`10`. | No (default: `1`) |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | Server-side `ended_ttl`: how long ended activities linger before cleanup (Go duration). | No (default: `15m`) |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Server-side `stale_ttl`: auto-expiry for stuck/abandoned activities (Go duration). | No (default: `60m`) |
| `PUSHWARD_END_DELAY` | `pushward.end_delay` | Delay before phase 1 (terminal `ONGOING` frame) of the two-phase end (Go duration). | No (default: `5s`) |
| `PUSHWARD_END_DISPLAY_TIME` | `pushward.end_display_time` | How long the terminal frame shows before the `ENDED` frame is sent (Go duration). | No (default: `4s`) |

### Polling

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_POLL_INTERVAL` | `polling.update_interval` | How often progress updates are sent / debounce interval for same-state ticks (Go duration). Validated to be **≥ 2s**. | No (default: `5s`) |

> State transitions (start, pause, finish, fail, cancel) are pushed immediately when MQTT delivers them; the poll interval only throttles in-progress progress frames and suppresses byte-identical content.

### TLS verification

Bambu Lab printers serve a self-signed certificate with no public PKI. The bridge picks one of three modes, in precedence order:

1. **Fingerprint pin (recommended)** — set `bambulab.tls.cert_fingerprint_sha256` (or `PUSHWARD_BAMBULAB_CERT_FINGERPRINT`). TLS then verifies the cert with a constant-time fingerprint comparison. Extract the fingerprint with:

   ```bash
   openssl s_client -connect <printer-ip>:8883 </dev/null 2>/dev/null \
     | openssl x509 -fingerprint -sha256 -noout
   ```

   Note: the fingerprint changes if the printer regenerates its cert (some firmware updates do this).
2. **Trust on first use (default)** — when no fingerprint is set and `insecure_skip_verify` is `false`, the bridge dials the printer once, captures the leaf cert's fingerprint, logs it, and pins it for the live MQTT connection.
3. **Skip verification** — set `bambulab.tls.insecure_skip_verify: true` to accept any cert. Logs a warning at startup. Only consulted when no fingerprint is set.

### Example config file

```yaml
bambulab:
  host: ""           # set PUSHWARD_BAMBULAB_HOST
  access_code: ""    # set PUSHWARD_BAMBULAB_ACCESS_CODE
  serial: ""         # set PUSHWARD_BAMBULAB_SERIAL
  tls:
    cert_fingerprint_sha256: ""   # or set PUSHWARD_BAMBULAB_CERT_FINGERPRINT
    # insecure_skip_verify: true  # last resort; accepts any cert

pushward:
  url: ""            # set PUSHWARD_URL (https://api.pushward.app)
  api_key: ""        # set PUSHWARD_API_KEY (hlk_..., activity:manage scope)
  priority: 1
  cleanup_delay: 15m
  stale_timeout: 60m

polling:
  update_interval: 5s
```

## Live Activity mapping

Each `gcode_state` maps to one frame of the `generic` Live Activity template. The subtitle combines the print filename and nozzle temperature (`NN/NN°C`) joined by ` · `.

| Printer state | Activity state | State text | Icon | Color |
|---|---|---|---|---|
| `PREPARE` | `ongoing` | `Preparing...` | `arrow.triangle.2.circlepath` | blue |
| `RUNNING` | `ongoing` | `Layer N/M` (or `N%`) | `printer.fill` | blue |
| `PAUSE` | `ongoing` | `Paused` | `pause.circle.fill` | orange |
| `FINISH` | `ended` (two-phase) | `Complete` | `checkmark.circle.fill` | green |
| `FAILED` | `ended` (two-phase) | `Failed` | `xmark.circle.fill` | red |
| `IDLE` (while tracking) | `ended` | `Cancelled` | `xmark.circle.fill` | orange |
| `SIGINT`/`SIGTERM` (while tracking) | `ended` | `Interrupted` | `xmark.circle.fill` | orange |

## Development

Run all commands from the repository root (`pushward-integrations/`), which is a Go workspace.

```bash
# Build
go build ./bambulab/cmd/pushward-bambulab

# Run with a config file (env vars override YAML)
./pushward-bambulab -config bambulab/config.example.yml

# Test (matches CI: race + verbose)
go test ./bambulab/... ./shared/... -race -count=1 -v

# Lint (matches CI)
golangci-lint run
```

### Docker build

The Docker build context is the **repository root** (not the bridge directory) so the Dockerfile can `COPY shared/`:

```bash
# Build (default Go toolchain ARG is 1.26.4)
docker build -f bambulab/Dockerfile -t pushward-bambulab .

# Pin the Go toolchain to match go.mod
docker build --build-arg GO_VERSION=1.26.4 -f bambulab/Dockerfile -t pushward-bambulab .
```

## CI/CD & Releases

- **CI** runs on every push/PR touching `bambulab/**`, `shared/**`, or the workflow file: Go tests (`-race -count=1 -v`), lint, and an image build.
- **Bridges are versioned independently.** Tag format: `bambulab/v<X.Y.Z>`. Pushing the tag triggers the release pipeline and a GitHub Release with auto-generated notes.
- Images are published to **GHCR only** (`ghcr.io/mac-lucky/pushward-bambulab`); Docker Hub publishing is disabled for this bridge.

| Trigger | Tags published |
|---|---|
| Pull request | none (build only) |
| Push to `main` | `:main`, `:main-<sha>` |
| Git tag `bambulab/v<X.Y.Z>` | `:X.Y.Z`, `:X.Y`, `:latest` (and `:X` once X ≥ 1) |

`:latest` only moves on a tagged release.

## Server compatibility

This bridge is an outbound REST client of [pushward-server](https://pushward.app). It targets the Activities API surface — `POST /activities` and `PATCH /activities/{slug}` with `Authorization: Bearer <hlk_ key>` — defined by the server's `openapi.yaml` (the contract owner). The shared `pushward.Client` is hand-written; it tracks the server's MAJOR.MINOR API. Patch releases of this bridge are bridge-only fixes and require no server change.

## Troubleshooting

Logs are structured JSON on stdout at `Info` level. With Docker: `docker logs -f pushward-bambulab`.

| Symptom | Likely cause / fix |
|---|---|
| `failed to connect to printer, retrying` (every 30s) | Printer is off, unreachable, or LAN/Developer Mode is disabled. The bridge retries until it appears — power on the printer or fix the network. |
| `MQTT connect` auth errors | Wrong `access_code`. Re-read it from printer Settings → WLAN. |
| `peer cert fingerprint mismatch` | The pinned `cert_fingerprint_sha256` no longer matches (printer regenerated its cert). Re-extract the fingerprint, or clear it to fall back to auto-pin. |
| `BambuLab TLS verification disabled via insecure_skip_verify` (warning) | Expected only if you set `insecure_skip_verify: true`. Prefer fingerprint pinning. |
| No activity appears on iPhone | Check `PUSHWARD_URL`/`PUSHWARD_API_KEY`, that the key has `activity:manage` scope, and that the iOS app is installed and subscribed. |
| `polling.update_interval must be >= 2s` | Raise `update_interval` (or `PUSHWARD_POLL_INTERVAL`) to at least `2s`. |
| `pushward.priority must be 0-10` | Set `priority` within `0`–`10`. |

## Requirements

- Go **1.26+** (the module declares `go 1.26.4`).
- A reachable Bambu Lab printer (MQTT/TLS on port `8883`) and a running PushWard server.

## License

Part of the [pushward-integrations](https://github.com/mac-lucky/pushward-integrations) repository — see the repository for license terms.
