# pushward-sabnzbd

Bridges SABnzbd download progress to [PushWard](https://github.com/mac-lucky/pushward-server) Live Activities on iOS.

Exposes a webhook endpoint that SABnzbd calls when an NZB is added, then polls the SABnzbd API through downloading and post-processing phases — displaying real-time progress on your iPhone's Dynamic Island and Lock Screen.

## Features

- **Multi-file tracking** — shows the current filename and a `+N more` count when multiple NZBs are queued
- **Live ETA countdown** — remaining time displayed on the Dynamic Island
- **Download speed** — real-time MB/s readout as the activity state
- **Post-processing phases** — Verifying, Repairing, Extracting, Moving each shown with a distinct icon
- **Completion summary** — total size, duration, and post-processing time (e.g. `1.2 GB in 5m 30s · PP: 45s`)
- **Paused state** — reflects SABnzbd pause/resume on the Live Activity
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
| `PUSHWARD_URL` | `pushward.url` | PushWard server URL | Yes |
| `PUSHWARD_API_KEY` | `pushward.api_key` | PushWard integration key (`hlk_`) | Yes |
| `PUSHWARD_SERVER_ADDRESS` | `server.address` | HTTP listen address (default: `:8090`) | No |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Activity priority 0-10 (default: `1`) | No |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | Delay before ENDED (default: `15m`) | No |
| `PUSHWARD_POLL_INTERVAL` | `polling.interval` | Poll interval (default: `1s`) | No |

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
      - PUSHWARD_URL=https://pushward.macluckylab.com
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

1. **Webhook** — SABnzbd sends a POST to `/webhook` when an NZB is added
2. **Activity creation** — auto-creates the `sabnzbd` activity on PushWard if it doesn't exist
3. **Wait for start** — polls the queue for up to 60s waiting for an active download
4. **Download tracking** — polls the queue every 1s, showing progress bar, speed, ETA, and current filename(s)
5. **Post-processing** — tracks Verifying, Repairing, Extracting, and Moving phases with status-specific icons
6. **Queue continuation** — if more downloads appear in the queue, loops back to step 4
7. **Summary** — shows completion stats (total size, duration, post-processing time), then ends after the cleanup delay
