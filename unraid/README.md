# pushward-unraid

Unraid bridge for [PushWard](https://pushward.app). Polls Unraid's GraphQL API for array state, subscribes to the notifications WebSocket, creates iOS Live Activities for parity checks and array transitions, and forwards every Unraid notification to the PushWard notification API.

This directory is the **source** for the binary that ships in the [`pushward-unraid-plugin`](https://github.com/mac-lucky/pushward-unraid-plugin) Unraid plugin.

## Install on Unraid

In the Unraid web UI: **Plugins → Install Plugin** and paste:

```
https://github.com/mac-lucky/pushward-unraid-plugin/raw/main/pushward-unraid.plg
```

Configure via **Settings → PushWard** (the only required fields are the Unraid API key and the PushWard integration key — `host` defaults to `localhost`). The plugin auto-starts on boot.

## Features

- **Parity check tracking** — creates a Live Activity when a parity check starts, updates progress every 30s, and ends with a "Parity Valid" confirmation
- **Array state monitoring** — renders a two-phase end activity on `STOPPED → STARTED` and `STARTED → STOPPED` transitions (Unraid's SDL has no `STARTING`/`STOPPING` intermediate states)
- **All Unraid notifications forwarded** — every `notificationAdded` event is forwarded to the PushWard notification API with interruption level mapped from Unraid importance (`ALERT`/`WARNING` → active, `INFO`/other → passive). SDL values are uppercase.
- **Two-phase activity end** — activities show final content on Dynamic Island before dismissing
- **Auto-reconnect** — notification WebSocket reconnects with exponential backoff + jitter (capped at 60s)
- **Array state polled, not subscribed** — `subscription arraySubscription` returns `Cannot return null for non-nullable field` on Unraid v4.x (server-side bug), so the bridge polls `query { array { ... } }` every 10s instead

## Prerequisites

- A running PushWard server
- The PushWard iOS app
- A PushWard integration key (`hlk_` prefix) with `activity:manage` scope
- An Unraid server with the GraphQL API reachable (served by nginx on port 80 / 443 at path `/graphql` — there is no dedicated listener on 3001)
- An Unraid API key (see below)

### Creating the Unraid API key

In the Unraid web UI, go to **Settings → Management Access → API Keys** tab and click **Create API Key**.

The bridge is read-only — it runs one GraphQL query and one subscription, never mutations. Grant the key **read** permission on:

| Resource | Why |
|---|---|
| `array` | `query { array { state parityCheckStatus { status progress } } }` polled every 10s |
| `notifications` | All Unraid notifications (SMART, UPS, Docker, user scripts, etc.) surface here via the `notificationAdded` subscription |

Any built-in role that covers these resources (e.g. `viewer`) works too. Avoid granting write/admin scopes — the bridge doesn't need them.

Copy the key when it's shown (it's only displayed once) and set it as `PUSHWARD_UNRAID_API_KEY`.

## Configuration

When installed via the plugin, configuration is written to `/boot/config/plugins/pushward-unraid/pushward-unraid.cfg` from the Settings UI. To run the binary directly during development, create a `config.yml` (see [`config.example.yml`](config.example.yml)):

```yaml
unraid:
  host: "localhost"           # nginx serves /graphql locally on Unraid
  port: 80                    # 80 (or 443 with use_tls)
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
| `PUSHWARD_UNRAID_HOST` | Unraid WebSocket host (defaults to `localhost`) |
| `PUSHWARD_UNRAID_PORT` | GraphQL WebSocket port |
| `PUSHWARD_UNRAID_API_KEY` | Unraid API key |
| `PUSHWARD_UNRAID_USE_TLS` | `true` to use `wss://` |
| `PUSHWARD_UNRAID_SERVER_NAME` | Display name in activity subtitles |
| `PUSHWARD_URL` | PushWard server URL |
| `PUSHWARD_API_KEY` | PushWard integration key |
| `PUSHWARD_PRIORITY` | Activity priority (integer) |
| `PUSHWARD_CLEANUP_DELAY` | Time before ended activities are cleaned up (e.g. `15m`) |
| `PUSHWARD_STALE_TIMEOUT` | Time before idle activities are marked stale (e.g. `24h`) |

## Build & Run (development)

```bash
go build ./unraid/cmd/pushward-unraid
./pushward-unraid -config unraid/config.example.yml
```

Releases for the plugin are produced by tagging `unraid-vX.Y.Z`, which triggers `.github/workflows/unraid-ci-cd.yml` to build a static `linux/amd64` binary and attach `pushward-unraid_<X.Y.Z>_linux_x86_64.tar.gz` (+ `.sha256`) to a GitHub Release.

## Activities

Live Activities are used for stateful, user-visible progress:

| Event | Slug | Template | Icon | Color |
|---|---|---|---|---|
| Parity check running | `unraid-parity` | `generic` | `arrow.triangle.2.circlepath` | blue |
| Parity check complete | `unraid-parity` | `generic` | `checkmark.circle.fill` | green |
| Array started (from STOPPED) | `unraid-array` | `generic` | `checkmark.circle.fill` | green |
| Array stopped (from STARTED) | `unraid-array` | `generic` | `checkmark.circle.fill` | green |

## Notifications

Every Unraid `notificationAdded` event is forwarded to the PushWard notification API (`POST /notifications`) with `source: unraid`, `thread_id: unraid`, and a stable `collapse_id` derived from the subject. Interruption level is mapped from Unraid `importance`:

| Importance | Level |
|---|---|
| `ALERT` | active |
| `WARNING` | active |
| `INFO` / other | passive |

All notifications set `push: true` — iOS's interruption level handles quiet delivery for the passive tier.

## Tests

```bash
go test ./unraid/... -v -count=1
```
