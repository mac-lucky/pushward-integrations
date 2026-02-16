# pushward-sabnzbd

Bridges SABnzbd download progress to [PushWard](https://github.com/mac-lucky/pushward-server) Live Activities on iOS.

Exposes a webhook endpoint that SABnzbd calls when an NZB is added, then polls the SABnzbd API through downloading and post-processing phases — displaying real-time progress on your iPhone's Dynamic Island and Lock Screen.

## Prerequisites

- A running PushWard server
- A PushWard activity and integration key (`hlk_` prefix)
- A SABnzbd instance with API access
- The PushWard iOS app subscribed to the activity

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

## How It Works

1. **Webhook**: SABnzbd sends a POST to `/webhook` when an NZB is added
2. **Wait for start**: Polls queue every 1s for up to 60s waiting for the download to begin
3. **Download tracking**: Polls queue every 1s, sends progress updates with speed, ETA, and remaining size
4. **Post-processing**: Tracks unpacking/verification/repair phases with status-specific icons
5. **Summary**: Shows completion stats (total size, duration, avg speed), then ends after 15m
