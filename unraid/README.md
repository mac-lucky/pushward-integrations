# pushward-unraid

Unraid Live Activity bridge for [PushWard](https://pushward.app). Connects to Unraid's GraphQL WebSocket API and creates iOS Live Activities for parity checks, array state changes, and critical notifications (disk errors, UPS events).

## Features

- **Parity check tracking** — creates a Live Activity when a parity check starts, updates progress every 30s, and ends with a "Parity Valid" confirmation
- **Array state monitoring** — tracks Starting/Started/Stopping/Stopped transitions with color-coded status
- **Disk alerts** — SMART errors and disk-related notifications surface as error-severity activities
- **UPS events** — battery and UPS notifications with warning/error severity based on importance
- **Two-phase end** — activities show final content on Dynamic Island before dismissing
- **Auto-reconnect** — WebSocket subscriptions reconnect with 5s backoff on connection loss

## Prerequisites

- A running PushWard server
- The PushWard iOS app
- A PushWard integration key (`hlk_` prefix) with `activity:manage` scope
- An Unraid server with the GraphQL API enabled (port 3001 by default)
- An Unraid API key

## Configuration

Create a `config.yml` (see [`config.example.yml`](config.example.yml)):

```yaml
unraid:
  host: "unraid.example.com"  # Unraid WebSocket host
  port: 3001                  # GraphQL WebSocket port
  api_key: ""                 # Unraid API key
  server_name: "Unraid"       # Display name in activity subtitles
  use_tls: false              # Use wss:// instead of ws://

pushward:
  url: ""                     # PushWard server URL
  api_key: ""                 # Integration key (hlk_...)
  priority: 2
  cleanup_delay: 15m
  stale_timeout: 24h
  end_delay: 5s
  end_display_time: 4s
```

### Environment Variables

All config values can be overridden via environment variables. The `PUSHWARD_` prefix is preferred; bare names are supported for compatibility.

| Variable | Description |
|---|---|
| `PUSHWARD_UNRAID_HOST` | Unraid WebSocket host |
| `PUSHWARD_UNRAID_PORT` | GraphQL WebSocket port |
| `PUSHWARD_UNRAID_API_KEY` | Unraid API key |
| `PUSHWARD_URL` | PushWard server URL |
| `PUSHWARD_API_KEY` | PushWard integration key |
| `PUSHWARD_PRIORITY` | Activity priority (integer) |
| `PUSHWARD_CLEANUP_DELAY` | Time before ended activities are cleaned up (e.g. `15m`) |
| `PUSHWARD_STALE_TIMEOUT` | Time before idle activities are marked stale (e.g. `24h`) |

## Build & Run

```bash
# Build
go build ./unraid/cmd/pushward-unraid

# Run
./pushward-unraid -config unraid/config.example.yml
```

## Docker

```bash
# Build (context is repo root, not the unraid dir)
docker build -f unraid/Dockerfile -t pushward-unraid .

# Run
docker run -v /path/to/config.yml:/config/config.yml pushward-unraid
```

Or with environment variables:

```bash
docker run \
  -e PUSHWARD_UNRAID_HOST=unraid.local \
  -e PUSHWARD_UNRAID_API_KEY=your-unraid-key \
  -e PUSHWARD_URL=https://api.pushward.app \
  -e PUSHWARD_API_KEY=hlk_your_key \
  pushward-unraid
```

## Activities

| Event | Slug | Template | Icon | Color |
|---|---|---|---|---|
| Parity check running | `unraid-parity` | `generic` | `arrow.triangle.2.circlepath` | blue |
| Parity check complete | `unraid-parity` | `generic` | `checkmark.circle.fill` | green |
| Array starting | `unraid-array` | `generic` | `arrow.triangle.2.circlepath` | blue |
| Array started | `unraid-array` | `generic` | `checkmark.circle.fill` | green |
| Array stopping | `unraid-array` | `generic` | `arrow.triangle.2.circlepath` | orange |
| Array stopped | `unraid-array` | `generic` | `checkmark.circle.fill` | green |
| Disk/SMART error | `unraid-disk-<subject>` | `alert` | `exclamationmark.octagon.fill` | red |
| UPS warning | `unraid-ups` | `alert` | `bolt.slash.fill` | orange |
| UPS alert | `unraid-ups` | `alert` | `bolt.slash.fill` | red |

## Tests

```bash
go test ./unraid/... -v -count=1
```
