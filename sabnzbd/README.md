# pushward-sabnzbd

Bridges SABnzbd download progress to [PushWard](https://github.com/mac-lucky/pushward-server) Live Activities on iOS.

Exposes a webhook endpoint that SABnzbd calls when an NZB is added, then polls the SABnzbd API through downloading and post-processing phases — displaying real-time progress on your iPhone's Dynamic Island and Lock Screen.

## Features

- **Multi-file tracking** — shows the current filename and a `+N more` count when multiple NZBs are queued
- **Live ETA countdown** — remaining time displayed on the Dynamic Island
- **Download speed** — real-time MB/s readout with running average as the activity state
- **Post-processing phases** — Verifying, Repairing, Extracting, Moving each shown with a distinct icon
- **Completion summary** — total size, avg speed, and unpack time (e.g. `Done · 1.2 GB · 45 MB/s avg · unpack 2m 3s`)
- **Two-phase end** — sends ONGOING with final content first (for push-update token delivery), then ENDED to dismiss
- **Resume on startup** — checks SABnzbd for active downloads/post-processing and resumes tracking automatically
- **Paused state** — reflects SABnzbd pause/resume on the Live Activity
- **Webhook secret** — optional `X-Webhook-Secret` header validation
- **Auto-activity management** — creates the PushWard activity on first webhook, no manual setup needed
- **Retry with backoff** — PushWard API calls retry up to 5 times with exponential backoff
- **Graceful shutdown** — waits for active tracking to finish on SIGINT/SIGTERM

## Prerequisites

- A running PushWard server
- A PushWard integration key (`hlk_` prefix)
- A SABnzbd instance with API access
- The PushWard iOS app subscribed to the `sabnzbd` activity

## Configuration

All settings can be provided via YAML config file or environment variables. Environment variables take precedence.

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_SABNZBD_URL` | `sabnzbd.url` | SABnzbd API URL | Yes |
| `PUSHWARD_SABNZBD_API_KEY` | `sabnzbd.api_key` | SABnzbd API key | Yes |
| `PUSHWARD_SABNZBD_WEBHOOK_SECRET` | `sabnzbd.webhook_secret` | Webhook secret for `X-Webhook-Secret` validation | No |
| `PUSHWARD_URL` | `pushward.url` | PushWard server URL | Yes |
| `PUSHWARD_API_KEY` | `pushward.api_key` | PushWard integration key (`hlk_`) | Yes |
| `PUSHWARD_SERVER_ADDRESS` | `server.address` | HTTP listen address (default: `:8090`) | No |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Activity priority 0-10 (default: `1`) | No |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Server-side stale TTL (default: `30m`) | No |
| `PUSHWARD_END_DELAY` | `pushward.end_delay` | Delay before two-phase end Phase 1 (default: `5s`) | No |
| `PUSHWARD_END_DISPLAY_TIME` | `pushward.end_display_time` | Display time before ENDED dismissal (default: `4s`) | No |
| `PUSHWARD_POLL_INTERVAL` | `polling.interval` | Poll interval during tracking (default: `1s`) | No |

> **Note:** `PUSHWARD_CLEANUP_DELAY` is accepted by the shared config but not used by this integration. The activity slug `sabnzbd` is created with `ended_ttl=0` so it persists across sessions for reuse.

## Docker Compose

```yaml
services:
  pushward-sabnzbd:
    image: ghcr.io/mac-lucky/pushward-sabnzbd:latest
    ports:
      - "8090:8090"
    volumes:
      - ./config.yml:/config/config.yml:ro
    environment:
      - PUSHWARD_SABNZBD_URL=https://sabnzbd.example.com/api
      - PUSHWARD_SABNZBD_API_KEY=your_sabnzbd_api_key
      - PUSHWARD_URL=https://api.pushward.app
      - PUSHWARD_API_KEY=hlk_xxxxxxxxxxxx
```

## SABnzbd Webhook Setup

In SABnzbd, go to **Config > Notifications** and add a notification script or use the **Notification URL** feature to POST to:

```
http://<pushward-sabnzbd-host>:8090/webhook
```

Set it to trigger on **NZB added** events.

## Endpoints

| Method | Path | Description |
|---|---|---|
| POST | `/webhook` | Trigger download tracking (called by SABnzbd) |
| GET | `/health` | Health check (returns `ok`) |

## How It Works

1. **Startup** — checks SABnzbd for active downloads or post-processing; if found, resumes tracking immediately. Otherwise, sends ENDED to dismiss any stale activity from a previous crash
2. **Webhook** — SABnzbd sends a POST to `/webhook` when an NZB is added
3. **Activity creation** — auto-creates the `sabnzbd` activity on PushWard (with `ended_ttl=0` so the slug persists, and `stale_ttl=30m`)
4. **Wait for start** — polls the queue for up to 60s waiting for an active download
5. **Download tracking** — polls the queue every 1s, showing progress bar, speed, ETA, and current filename(s)
6. **Post-processing** — tracks Verifying, Repairing, Extracting, and Moving phases with status-specific icons
7. **Queue continuation** — if more downloads appear in the queue, loops back to step 5
8. **Summary** — shows completion stats (e.g. `Done · 1.2 GB · 45 MB/s avg · unpack 2m 3s`), subtitle shows the downloaded filename
9. **Two-phase end** — after the end delay (default 5s), sends ONGOING with final content so the push-update token delivers it to the device, then after the display time (default 4s) sends ENDED to dismiss the Live Activity. Skipped for resumed sessions (sends ENDED directly)
