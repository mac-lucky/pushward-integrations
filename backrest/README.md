# pushward-backrest

Turns a running [Backrest](https://github.com/garethgeorge/backrest) backup into an iOS Live
Activity with a bar that actually moves: bytes transferred, transfer rate, and an ETA that counts
down on the Lock Screen between polls.

Backrest can already POST its hooks at the PushWard relay, and for prune or check notifications
that is enough. But a hook fires at a moment in time, so a hook-driven activity can only say
"backing up" and then "done" - it has no way to show a 40-minute backup getting there. This bridge
polls the Backrest API instead, which is where the progress numbers live.

## What it shows

| Backrest operation | Rendering |
|---|---|
| Backup, running | Progress bar, `12.4 GB of 48.1 GB · 14.2 MB/s`, live ETA |
| Backup, finished | `Complete · 79 MB · 785 files · 10s`, green |
| Backup, unreadable files | Log view listing each file restic could not read, orange |
| Backup, failed | Red, bar left where restic stopped |
| Prune / check, running | Log view of the restic output, refreshed as it grows |
| Prune / check, finished | `Pruned` / `Check Passed`, or the failing output |

Prune and check carry no percent-done anywhere in the Backrest protocol - only a log reference -
so they get a log view rather than an invented bar.

The bridge only reads. It never starts, cancels, or reconfigures anything in Backrest.

## Quickstart (Docker)

The build context **must be the repo root** so the Dockerfile can `COPY shared/`.

```bash
docker build -f backrest/Dockerfile -t pushward-backrest .

docker run \
  -e PUSHWARD_URL=https://api.pushward.app \
  -e PUSHWARD_API_KEY=hlk_your_key_here \
  -e PUSHWARD_BACKREST_URL=http://backrest:9898 \
  pushward-backrest
```

### Docker Compose

Run it next to Backrest so it can reach the API on the container network:

```yaml
services:
  pushward-backrest:
    image: ghcr.io/mac-lucky/pushward-backrest:latest
    restart: unless-stopped
    environment:
      - PUSHWARD_URL=https://api.pushward.app
      - PUSHWARD_API_KEY=hlk_your_key_here
      - PUSHWARD_BACKREST_URL=http://backrest:9898
      # Only if Backrest has authentication enabled:
      - PUSHWARD_BACKREST_USERNAME=your-user
      - PUSHWARD_BACKREST_PASSWORD=your-password
```

The image runs as non-root UID 1000 and exposes no ports - it only makes outbound calls.

## Configuration

Settings come from a YAML file (`-config`, default `config.yml`) **or** environment variables.
**Environment variables win.** See [`config.example.yml`](./config.example.yml) for the annotated
version.

| Env variable | Config key | Description | Default |
|---|---|---|---|
| `PUSHWARD_BACKREST_URL` | `backrest.url` | Base URL of the Backrest web UI / API | required |
| `PUSHWARD_BACKREST_USERNAME` | `backrest.username` | HTTP Basic user | empty |
| `PUSHWARD_BACKREST_PASSWORD` | `backrest.password` | HTTP Basic password | empty |
| `PUSHWARD_BACKREST_TOKEN` | `backrest.token` | Bearer JWT, instead of Basic | empty |
| `PUSHWARD_BACKREST_TIMEOUT` | `backrest.timeout` | Per-request timeout | `15s` |
| `PUSHWARD_URL` | `pushward.url` | PushWard server base URL | required |
| `PUSHWARD_API_KEY` | `pushward.api_key` | Integration key (`hlk_`) | required |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Activity priority, 0-10 | `1` |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | The activity's `ended_ttl`: how long a finished card lingers, and how long it stays on the Lock Screen | `10m` |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Silence after which the server ends the activity itself. The keep-alive is half of it | `30m` |
| `PUSHWARD_LOG_LEVEL` | _(env only)_ | `debug`, `info`, `warn` or `error`. Read before the config file, so it works even when config loading is what failed | `info` |
| `PUSHWARD_POLL_INTERVAL` | `polling.interval` | Poll interval while something is running | `5s` |
| `PUSHWARD_POLL_IDLE` | `polling.idle_interval` | Poll interval while idle | `30s` |
| `PUSHWARD_BACKREST_LAST_N` | `polling.last_n` | Operations requested per poll | `50` |
| `PUSHWARD_BACKREST_LIVE_PROGRESS` | `render.live_progress` | Animate the bar and ETA between polls | `true` |
| `PUSHWARD_BACKREST_LOGS` | `render.logs` | Render prune/check as a log view | `true` |
| `PUSHWARD_BACKREST_MAX_ETA` | `render.max_eta` | Longest ETA still worth anchoring the bar to | `168h` |

Backrest's auth middleware accepts HTTP Basic or a bearer JWT, and lets every request through when
authentication is disabled. Leaving all three credential fields empty is a supported setup, not an
incomplete one.

## Build and run from source

```bash
# From the workspace root (uses go.work)
go build ./backrest/cmd/pushward-backrest

# From inside backrest/
go build -o pushward-backrest ./cmd/pushward-backrest

# Minimum run with no config file
PUSHWARD_URL=https://api.pushward.app \
PUSHWARD_API_KEY=hlk_your_key_here \
PUSHWARD_BACKREST_URL=http://localhost:9898 \
./pushward-backrest
```

```bash
go test ./backrest/... -race -count=1
```

## Running alongside the relay

If your Backrest also posts hooks at the PushWard relay, drop the `CONDITION_SNAPSHOT_*` conditions
from your **plan** hooks once this bridge is running, or every backup produces two Live Activities.
Leave the repo-level hooks alone: `CONDITION_PRUNE_*`, `CONDITION_CHECK_*` and `CONDITION_ANY_ERROR`
are how you keep push notifications for repo maintenance and failures.

The two use different slug schemes, so they will not overwrite each other's activities - they will
simply both be on screen.

## How it works

Each poll asks for the last 50 operations (`POST /v1.Backrest/GetOperations`). That is a unary
ConnectRPC call, which over the wire is an ordinary JSON POST, so there is no generated client and
no protobuf dependency. Log text comes from `GetLogs`, which does stream, and the bridge walks the
Connect envelopes itself.

Backrest publishes no time-remaining field, so the ETA is derived: the byte counter is sampled on
each poll and smoothed into a transfer rate. The resulting end date is only re-sent when the
estimate moves materially, because every new anchor restarts the animation on the phone and a
constantly nudged one reads as a stutter.

Anything past `max_eta` is dropped rather than anchored, since a backup that stalls in its first
minutes can produce an estimate of any size. The ceiling defaults to a week, not a working day: a
1.5 TB first run at 9 MB/s is roughly two days out, and that countdown is worth reading even though
the bar itself only creeps.

A frame is only pushed when the bar has moved more than 2%, the state line has changed, the ETA has
been re-anchored, or a prune or check has written new output - plus a keep-alive at half the
configured `stale_timeout` so the server does not end the activity as stale during a long, quiet
stretch. A prune reports no progress at all, so that last output is the only thing its card has to
say; the log behind it is re-read every 15 seconds rather than on every poll.

The first poll after startup is special: it records what Backrest has already finished without
announcing any of it. Without that, starting the bridge would fire an activity for every backup in
the recent history.

The exception is anything that finished within `stale_timeout` of startup, which is announced and
closed normally. That covers the case where the bridge is restarted mid-backup - a container
rollout, say - and the backup completes while it is down: without it the card the previous process
left behind would sit on the Lock Screen showing stale progress until the server timed it out.
