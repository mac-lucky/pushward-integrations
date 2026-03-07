# pushward-bambulab

Bridges a BambuLab 3D printer to [PushWard](https://github.com/mac-lucky/pushward-server) Live Activities on iOS.

Connects directly to the printer via local MQTT over LAN — no cloud dependency. Print progress, layer count, remaining time, and nozzle temperature appear as a Live Activity on your iPhone's Dynamic Island and Lock Screen. Paused, failed, and cancelled prints are reflected in real time.

## Features

- **Real-time print tracking** — progress bar, layer count, remaining time countdown, and nozzle temperature
- **Full print lifecycle** — preparing, printing, paused, finished, failed, and cancelled states
- **Local MQTT** — connects directly to the printer over LAN, no BambuLab cloud required
- **Delta state merging** — correctly handles P1/A1 series delta-only MQTT updates
- **Auto-resume** — detects in-progress prints on startup and resumes tracking
- **Two-phase end** — shows completion/failure state on Dynamic Island before dismissing
- **Retry with backoff** — PushWard API calls retry up to 5 times with exponential backoff
- **Graceful shutdown** — ends active activity on SIGINT/SIGTERM

## Prerequisites

- A running PushWard server
- A PushWard integration key (`hlk_` prefix)
- A BambuLab printer with **Developer Mode** or **LAN-only mode** enabled
- The 8-character **Access Code** from the printer (Settings > WLAN)
- The printer's **serial number** (15 characters)
- The printer's local **IP address** on your network
- The PushWard iOS app subscribed to the activity

## Configuration

All settings can be provided via YAML config file or environment variables. Environment variables take precedence.

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_URL` | `pushward.url` | PushWard server URL | Yes |
| `PUSHWARD_API_KEY` | `pushward.api_key` | PushWard integration key (`hlk_`) | Yes |
| `PUSHWARD_BAMBULAB_HOST` | `bambulab.host` | Printer IP address or hostname | Yes |
| `PUSHWARD_BAMBULAB_ACCESS_CODE` | `bambulab.access_code` | 8-character access code from printer | Yes |
| `PUSHWARD_BAMBULAB_SERIAL` | `bambulab.serial` | Printer serial number | Yes |
| `PUSHWARD_POLL_INTERVAL` | `polling.update_interval` | Progress update interval (default: `5s`) | No |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Activity priority 0-10 (default: `1`) | No |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | Server-side `ended_ttl` for ended activities (default: `15m`) | No |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Server-side `stale_ttl` for stuck activities (default: `60m`) | No |
| `PUSHWARD_END_DELAY` | `pushward.end_delay` | Delay before phase 1 of two-phase end (default: `5s`) | No |
| `PUSHWARD_END_DISPLAY_TIME` | `pushward.end_display_time` | Display time before sending ENDED (default: `4s`) | No |

## Docker Compose

```yaml
services:
  pushward-bambulab:
    image: ghcr.io/mac-lucky/pushward-bambulab:latest
    volumes:
      - ./config.yml:/config/config.yml:ro
    environment:
      - PUSHWARD_URL=https://api.pushward.app
      - PUSHWARD_API_KEY=hlk_xxxxxxxxxxxx
      - PUSHWARD_BAMBULAB_HOST=192.168.1.100
      - PUSHWARD_BAMBULAB_ACCESS_CODE=12345678
      - PUSHWARD_BAMBULAB_SERIAL=01A00A000000000
```

> No ports are exposed — the integration connects outbound to the printer via MQTT and to the PushWard API via HTTPS.

## Print Lifecycle

| Printer State | Activity State | State Text | Icon | Color |
|---|---|---|---|---|
| `PREPARE` | ONGOING | Preparing... | `arrow.triangle.2.circlepath` | blue |
| `RUNNING` | ONGOING | Layer 12/150 | `printer.fill` | blue |
| `PAUSE` | ONGOING | Paused | `pause.circle.fill` | orange |
| `FINISH` | ENDED (two-phase) | Complete | `checkmark.circle.fill` | green |
| `FAILED` | ENDED (two-phase) | Failed | `xmark.circle.fill` | red |
| `IDLE` (from tracking) | ENDED | Cancelled | `xmark.circle.fill` | orange |

## How It Works

1. **MQTT connection** — connects to the printer via TLS on port 8883 (username `bblp`, self-signed cert) and subscribes to `device/<serial>/report`
2. **Initial state** — sends a `pushall` command to get a full state snapshot (important for P1/A1 series that otherwise only send deltas)
3. **Print detection** — watches for `PREPARE` or `RUNNING` states; auto-creates a PushWard activity when a print starts
4. **Progress updates** — sends debounced updates (default every 5s) with progress, layer count, remaining time, filename, and nozzle temperature
5. **Completion** — on `FINISH` or `FAILED`, uses a two-phase end: first sends the final content as `ONGOING` (so it appears on Dynamic Island), then sends `ENDED` to dismiss
6. **Auto-resume** — if the printer is already printing when the bridge starts, it immediately resumes tracking
