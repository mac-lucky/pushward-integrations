# pushward-unraid

Unraid bridge for [PushWard](https://pushward.app). Connects to Unraid's GraphQL WebSocket API, creates iOS Live Activities for parity checks and array state changes, and forwards every Unraid notification to the PushWard notification API.

## Features

- **Parity check tracking** — creates a Live Activity when a parity check starts, updates progress every 30s, and ends with a "Parity Valid" confirmation
- **Array state monitoring** — tracks Starting/Started/Stopping/Stopped transitions with color-coded status
- **All Unraid notifications forwarded** — every `notificationAdded` event is forwarded to the PushWard notification API with interruption level mapped from Unraid importance (`alert`/`warning` → active, `info`/`notice` → passive)
- **Two-phase activity end** — activities show final content on Dynamic Island before dismissing
- **Auto-reconnect** — WebSocket subscriptions reconnect with 5s backoff on connection loss

## Prerequisites

- A running PushWard server
- The PushWard iOS app
- A PushWard integration key (`hlk_` prefix) with `activity:manage` scope
- An Unraid server with the GraphQL API reachable (served by nginx on port 80 / 443 at path `/graphql` — there is no dedicated listener on 3001)
- An Unraid API key (see below)

### Creating the Unraid API key

In the Unraid web UI, go to **Settings → Management Access → API Keys** tab and click **Create API Key**.

The bridge is read-only — it only opens GraphQL subscriptions, never mutations. Grant the key **read** permission on:

| Resource | Why |
|---|---|
| `array` | Array state transitions (Starting/Started/Stopping/Stopped) and parity check progress/ETA |
| `disks` | Disk list, status, and temperature (part of the `arraySubscription` payload) |
| `notifications` | All Unraid notifications (SMART, UPS, Docker, user scripts, etc.) surface here via `notificationAdded` |

Any built-in role that covers these resources (e.g. `viewer`) works too. Avoid granting write/admin scopes — the bridge doesn't need them.

Copy the key when it's shown (it's only displayed once) and set it as `PUSHWARD_UNRAID_API_KEY`.

## Configuration

Create a `config.yml` (see [`config.example.yml`](config.example.yml)):

```yaml
unraid:
  host: "unraid.example.com"  # Unraid WebSocket host
  port: 80                    # nginx serves /graphql on 80 (or 443 with use_tls)
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

Live Activities are used for stateful, user-visible progress:

| Event | Slug | Template | Icon | Color |
|---|---|---|---|---|
| Parity check running | `unraid-parity` | `generic` | `arrow.triangle.2.circlepath` | blue |
| Parity check complete | `unraid-parity` | `generic` | `checkmark.circle.fill` | green |
| Array starting | `unraid-array` | `generic` | `arrow.triangle.2.circlepath` | blue |
| Array started | `unraid-array` | `generic` | `checkmark.circle.fill` | green |
| Array stopping | `unraid-array` | `generic` | `arrow.triangle.2.circlepath` | orange |
| Array stopped | `unraid-array` | `generic` | `checkmark.circle.fill` | green |

## Notifications

Every Unraid `notificationAdded` event is forwarded to the PushWard notification API (`POST /notifications`) with `source: unraid`, `thread_id: unraid`, and a stable `collapse_id` derived from the subject. Interruption level is mapped from Unraid `importance`:

| Importance | Level | Category |
|---|---|---|
| `alert` | active | critical |
| `warning` | active | warning |
| `info` / `notice` / other | passive | info |

All notifications set `push: true` — iOS's interruption level handles quiet delivery for the passive tier.

## Tests

```bash
go test ./unraid/... -v -count=1
```
