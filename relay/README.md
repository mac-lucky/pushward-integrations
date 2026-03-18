# pushward-relay

Multi-tenant webhook gateway that consolidates multiple providers into a single [PushWard](https://pushward.app) bridge with PostgreSQL shared state. Each tenant authenticates with their own `hlk_` integration key per request — no per-service API key configuration needed.

## Features

- **Multi-tenant** — tenants are identified by their `hlk_` integration key, extracted from every request by a shared auth middleware
- **13 providers** — Grafana, ArgoCD, Radarr, Sonarr, Jellyfin, Paperless-ngx, Changedetection.io, Unmanic, Proxmox, Overseerr, Uptime Kuma, Gatus, Backrest
- **PostgreSQL state** — persistent state store with automatic TTL cleanup for alert grouping, sync tracking, and download tracking
- **Per-tenant client pool** — LRU pool of PushWard API clients keyed by integration key (max 1000 concurrent tenants)
- **Rate limiting** — dual-layer: per-IP (5 req/s, burst 20) and per-key (1 req/s, burst 10)
- **Two-phase end** — activities show final content on Dynamic Island before dismissing (ArgoCD, Starr, Jellyfin, Paperless, Unmanic, Proxmox, Overseerr, Uptime Kuma, Gatus, Backrest)
- **Retry with backoff** — PushWard API calls retry up to 5 times with exponential backoff and 429 rate-limit handling
- **Graceful shutdown** — waits for in-flight requests on SIGINT/SIGTERM

## Prerequisites

- A running PushWard server
- A PostgreSQL database
- The PushWard iOS app
- At least one PushWard integration key (`hlk_` prefix) per tenant

## Providers

### Grafana

Receives Grafana alert webhooks. Groups alerts by `alertname`, worst severity drives icon/color.

| Route | `POST /grafana` |
|---|---|
| Template | `alert` |
| Auth | `Authorization: Bearer hlk_...` |
| Slug | `grafana-<sha256(alertname)[:6]>` |

**Events:** `firing` → ONGOING (red/orange/blue by severity), `resolved` → ENDED (green checkmark)

### ArgoCD

Receives ArgoCD sync webhooks via argocd-notifications. Maps sync progress to a 3-step pipeline.

| Route | `POST /argocd` |
|---|---|
| Template | `pipeline` |
| Auth | `Authorization: Bearer hlk_...` |
| Slug | `argocd-<sanitized-app-name>` |

**Events:** `sync-running` → Step 1/3 Syncing, `sync-succeeded` → Step 2/3 Rolling out, `deployed` → Step 3/3 Deployed, `sync-failed` → Sync Failed, `health-degraded` → Degraded (transient warning during rollout)

### Radarr / Sonarr

Receives Radarr and Sonarr webhooks. Tracks download lifecycle from grab to import.

| Route | `POST /radarr` / `POST /sonarr` |
|---|---|
| Template | `generic` (downloads), `alert` (health) |
| Auth | Basic Auth with `hlk_` key as password |
| Slug | `radarr-<downloadId>` / `sonarr-<downloadId>` |

**Events:**

| Event | State | Icon | Color |
|---|---|---|---|
| `Grab` | Grabbed | `arrow.down.circle` | blue |
| `ManualInteractionRequired` | Import Failed | `exclamationmark.triangle.fill` | orange |
| `Download` | Imported / Upgraded | `checkmark.circle.fill` | green |
| `Health` | (health message) | `exclamationmark.triangle.fill` / `.octagon.fill` | orange / red |
| `HealthRestored` | (restored message) | `checkmark.circle.fill` | green |
| `Test` | (provider-specific test activity) | varies | varies |

### Jellyfin

Receives Jellyfin webhook plugin notifications. Tracks playback progress, library additions, scheduled tasks, and auth failures.

| Route | `POST /jellyfin` |
|---|---|
| Template | `generic` (playback), `pipeline` (items/tasks), `alert` (auth failures) |
| Auth | `Authorization: Bearer hlk_...` |
| Slug | `jellyfin-<sha256(ItemId+UserName)[:10]>` (playback), `jellyfin-item-<hash>` (library), `jellyfin-task-<hash>` (tasks), `jellyfin-auth-<hash>` (auth) |

**Events:**

| Event | State | Icon | Color |
|---|---|---|---|
| `PlaybackStart` | Playing on (device) | `play.circle.fill` | blue |
| `PlaybackProgress` | Playing on (device) | `play.circle.fill` | blue |
| `PlaybackStop` | Watched on (device) | `checkmark.circle.fill` | green |
| `ItemAdded` | Added to library | `plus.circle.fill` | green |
| `ScheduledTaskStarted` | Running... | `arrow.triangle.2.circlepath` | blue |
| `ScheduledTaskCompleted` | Complete / Failed | `checkmark.circle.fill` / `xmark.circle.fill` | green / red |
| `AuthenticationFailure` | Failed login: user from IP | `lock.shield.fill` | red |
| `GenericUpdateNotification` | (provider-specific test activity) | varies | varies |

**Debounce:** `PlaybackProgress` updates within `progress_debounce` (default 30s) are skipped.

**Setup:** In Jellyfin, install the Webhook plugin. Add a Generic destination with URL `https://relay.example.com/jellyfin`. Under **Add Request Header**, set Key to `Authorization` and Value to `Bearer hlk_...`.

### Paperless-ngx

Receives document consumption webhooks. Users configure the JSON body via a Jinja2 template in the Paperless Workflows UI.

| Route | `POST /paperless` |
|---|---|
| Template | `generic` |
| Auth | `Authorization: Bearer hlk_...` |
| Slug | `paperless-<doc_id>` (added/updated), `paperless-<sha256(filename)[:4]>` (consumption started) |

**Events:**

| Event | State | Icon | Color |
|---|---|---|---|
| `added` | Processed | `doc.text.fill` | green |
| `updated` | Updated | `doc.text.fill` | green |
| `consumption_started` | Processing... | `arrow.triangle.2.circlepath` | blue |

**Setup:** In Paperless-ngx, go to Admin > Workflows. Create a workflow with trigger "Document Added" and action "Webhook". Set the URL to `https://relay.example.com/paperless`. Add an `Authorization: Bearer hlk_...` header. Use this body template:

```
{"event":"added","doc_id":{{doc_id}},"title":{{doc_title|tojson}},"correspondent":{{correspondent|tojson}},"document_type":{{document_type|tojson}},"doc_url":{{doc_url|tojson}},"filename":{{original_filename|tojson}},"tags":{{tag_name_list|tojson}}}
```

### Changedetection.io

Receives page change notifications. Users configure the JSON body via a Jinja2 template in Changedetection.io's notification settings.

| Route | `POST /changedetection` |
|---|---|
| Template | `alert` |
| Auth | `Authorization: Bearer hlk_...` |
| Slug | `cd-<sha256(url)[:8]>` |

**Events:** Single event type — page changed. Creates a fire-and-forget alert notification (ONGOING + immediate ENDED).

| Field | Value |
|---|---|
| State | Triggered text (or "Page changed") |
| Icon | `eye.fill` |
| Color | `#FF9500` (orange) |
| URL | `diff_url` |
| Secondary URL | `preview_url` |

**Setup:** In Changedetection.io, set the notification URL to:

```
posts://relay.example.com/changedetection?+Authorization=Bearer+hlk_...
```

Set `notification_format` to `custom` and use this body template:

```
{"url":{{watch_url|tojson}},"title":{{watch_title|tojson}},"tag":{{watch_tag|tojson}},"diff_url":{{diff_url|tojson}},"preview_url":{{preview_url|tojson}},"triggered_text":{{triggered_text|tojson}},"timestamp":{{notification_timestamp|tojson}}}
```

### Unmanic

Receives Apprise `json://` notifications from Unmanic on transcoding task completion or failure.

| Route | `POST /unmanic` |
|---|---|
| Template | `generic` |
| Auth | `Authorization: Bearer hlk_...` |
| Slug | `unmanic-<sha256(filename)[:8]>` |

**Events:**

| Type | State | Icon | Color |
|---|---|---|---|
| `success` | Complete | `checkmark.circle.fill` | green |
| `failure` | Failed | `xmark.circle.fill` | red |
| `info` | (provider-specific test activity) | varies | varies |

**Setup:** In Unmanic, go to Settings > Notifications. Add a notification URL:

```
jsons://relay.example.com/unmanic?+Authorization=Bearer+hlk_...
```

### Proxmox VE

Receives Proxmox VE notification webhooks for backup, replication, fencing, and package update events.

| Route | `POST /proxmox` |
|---|---|
| Template | `pipeline` (backup/replication), `alert` (fencing/package-updates) |
| Auth | `Authorization: Bearer hlk_...` |
| Slug | `proxmox-<type>-<target>` |

**Events:**

| Event | State | Icon | Color |
|---|---|---|---|
| `vzdump` (start) | Backup starting | `arrow.triangle.2.circlepath` | blue |
| `vzdump` (complete) | Backup complete | `checkmark.circle.fill` | green |
| `vzdump` (failed) | Backup failed | `xmark.circle.fill` | red |
| `replication` (start) | Replication starting | `arrow.triangle.2.circlepath` | blue |
| `replication` (complete) | Replication complete | `checkmark.circle.fill` | green |
| `replication` (failed) | Replication failed | `xmark.circle.fill` | red |
| `fencing` | Node fenced | `exclamationmark.triangle.fill` | red |
| `package-updates` | Updates available | `arrow.down.circle` | orange |
| `system` | (test notification) | varies | varies |

**Setup:** In Proxmox VE, go to Datacenter > Notifications. Add a webhook target with URL `https://relay.example.com/proxmox`. Set the `Authorization` header to `Bearer hlk_...`.

### Overseerr / Jellyseerr

Receives Overseerr/Jellyseerr media request webhooks. Tracks request lifecycle from pending to available.

| Route | `POST /overseerr` |
|---|---|
| Template | `pipeline` |
| Auth | `Authorization: Bearer hlk_...` |
| Slug | `overseerr-<mediaType>-<tmdbId>` |

**Events:**

| Event | State | Step | Color |
|---|---|---|---|
| `MEDIA_PENDING` | Requested | 1/4 | blue |
| `MEDIA_APPROVED` | Approved | 2/4 | blue |
| `MEDIA_AVAILABLE` | Available | 4/4 | green |
| `MEDIA_DECLINED` | Declined | - | red |
| `MEDIA_FAILED` | Failed | - | red |

**Setup:** In Overseerr/Jellyseerr, go to Settings > Notifications > Webhook. Set the Webhook URL to `https://relay.example.com/overseerr`. Set the `Authorization` header to `Bearer hlk_...`.

### Uptime Kuma

Receives Uptime Kuma monitor status webhooks. Maps monitor heartbeat status to alert notifications.

| Route | `POST /uptimekuma` |
|---|---|
| Template | `alert` |
| Auth | `Authorization: Bearer hlk_...` |
| Slug | `uptimekuma-<monitorId>` |

**Events:**

| Status | State | Icon | Color |
|---|---|---|---|
| `0` (DOWN) | Down | `xmark.circle.fill` | red |
| `1` (UP) | Up | `checkmark.circle.fill` | green |
| `2` (PENDING) | Pending | `clock.fill` | orange |
| `3` (MAINTENANCE) | (test notification) | varies | varies |

**Setup:** In Uptime Kuma, go to Settings > Notifications. Add a notification of type "Webhook" with URL `https://relay.example.com/uptimekuma`. Set the `Authorization` header to `Bearer hlk_...`.

### Gatus

Receives Gatus health check alert webhooks. Maps endpoint TRIGGERED/RESOLVED states to alert notifications.

| Route | `POST /gatus` |
|---|---|
| Template | `alert` |
| Auth | `Authorization: Bearer hlk_...` |
| Slug | `gatus-<sha256(group/endpoint_name)[:12]>` |

**Events:**

| Status | State | Icon | Color |
|---|---|---|---|
| `TRIGGERED` | (error details) | `exclamationmark.triangle.fill` | red |
| `RESOLVED` | Resolved | `checkmark.circle.fill` | green |

**Setup:** In your `gatus.yaml`, configure `alerting.custom`:

```yaml
alerting:
  custom:
    url: "https://relay.example.com/gatus"
    method: "POST"
    headers:
      Content-Type: "application/json"
      Authorization: "Bearer hlk_..."
    body: |
      {
        "endpoint_name": "[ENDPOINT_NAME]",
        "endpoint_group": "[ENDPOINT_GROUP]",
        "endpoint_url": "[ENDPOINT_URL]",
        "alert_description": "[ALERT_DESCRIPTION]",
        "status": "[ALERT_TRIGGERED_OR_RESOLVED]",
        "result_errors": "[RESULT_ERRORS]"
      }
```

Then reference `type: custom` in your endpoint alerts with `send-on-resolved: true`.

### Backrest

Receives Backrest backup operation webhooks for snapshot, prune, and check operations.

| Route | `POST /backrest` |
|---|---|
| Template | `generic` |
| Auth | `Authorization: Bearer hlk_...` |
| Slug | `backrest-<planId>-<operation>` |

**Events:**

| Condition | State | Icon | Color |
|---|---|---|---|
| `CONDITION_SNAPSHOT_START` | Snapshot starting | `arrow.triangle.2.circlepath` | blue |
| `CONDITION_SNAPSHOT_SUCCESS` | Snapshot complete | `checkmark.circle.fill` | green |
| `CONDITION_SNAPSHOT_WARNING` | Snapshot warning | `exclamationmark.triangle.fill` | orange |
| `CONDITION_SNAPSHOT_ERROR` | Snapshot failed | `xmark.circle.fill` | red |
| `CONDITION_PRUNE_START` | Prune starting | `arrow.triangle.2.circlepath` | blue |
| `CONDITION_PRUNE_SUCCESS` | Prune complete | `checkmark.circle.fill` | green |
| `CONDITION_PRUNE_ERROR` | Prune failed | `xmark.circle.fill` | red |
| `CONDITION_CHECK_START` | Check starting | `arrow.triangle.2.circlepath` | blue |
| `CONDITION_CHECK_SUCCESS` | Check complete | `checkmark.circle.fill` | green |
| `CONDITION_CHECK_ERROR` | Check failed | `xmark.circle.fill` | red |

**Setup:** In Backrest, go to Settings > Notifications. Add a webhook with URL `https://relay.example.com/backrest`. Set the `Authorization` header to `Bearer hlk_...`.

## Configuration

All settings can be provided via YAML config file (`-config` flag, default `config.yml`) or environment variables. Environment variables take precedence.

| Env Variable | Description | Required |
|---|---|---|
| `PUSHWARD_DATABASE_DSN` | PostgreSQL connection string | Yes |
| `PUSHWARD_URL` | PushWard server URL (also accepts `-pushward-url` flag) | Yes |
| `PUSHWARD_SERVER_ADDRESS` | HTTP listen address (default: `:8090`) | No |
| `PUSHWARD_GRAFANA_SEVERITY_LABEL` | Alert label for severity (default: `severity`) | No |
| `PUSHWARD_GRAFANA_DEFAULT_SEVERITY` | Fallback severity (default: `warning`) | No |
| `PUSHWARD_GRAFANA_DEFAULT_ICON` | Fallback icon (default: `exclamationmark.triangle.fill`) | No |
| `PUSHWARD_ARGOCD_URL` | ArgoCD UI URL for "View in ArgoCD" links | No |
| `PUSHWARD_SYNC_GRACE_PERIOD` | Skip no-op syncs within this window (default: `10s`) | No |
See [`config.example.yml`](./config.example.yml) for the full config with per-provider settings.

## Docker Compose

```yaml
services:
  pushward-relay:
    image: ghcr.io/mac-lucky/pushward-relay:latest
    ports:
      - "8090:8090"
    volumes:
      - ./config.yml:/config/config.yml:ro
    environment:
      - PUSHWARD_DATABASE_DSN=postgres://user:pass@db:5432/pushward_relay?sslmode=disable
      - PUSHWARD_URL=https://api.pushward.app
```

## Endpoints

| Method | Path | Description |
|---|---|---|
| POST | `/grafana` | Grafana alert webhooks |
| POST | `/argocd` | ArgoCD sync webhooks |
| POST | `/radarr` | Radarr webhooks |
| POST | `/sonarr` | Sonarr webhooks |
| POST | `/jellyfin` | Jellyfin webhook plugin |
| POST | `/paperless` | Paperless-ngx workflow webhooks |
| POST | `/changedetection` | Changedetection.io notifications |
| POST | `/unmanic` | Unmanic Apprise notifications |
| POST | `/proxmox` | Proxmox VE notification webhooks |
| POST | `/overseerr` | Overseerr/Jellyseerr media request webhooks |
| POST | `/uptimekuma` | Uptime Kuma monitor status webhooks |
| POST | `/gatus` | Gatus health check alert webhooks |
| POST | `/backrest` | Backrest backup/prune/check webhooks |
| GET | `/health` | Health check (returns `ok`) |

## How It Works

1. **Request arrives** — IP rate limiter checks the client IP against a per-IP token bucket (5 req/s, burst 20)
2. **Auth** — the `hlk_` integration key is extracted from `Authorization: Bearer` or Basic Auth password and stored in the request context
3. **Key rate limit** — a per-key token bucket (1 req/s, burst 10) prevents any single tenant from flooding the relay
4. **Provider handler** — the matched handler decodes the JSON payload, determines the event type, and maps it to a PushWard activity lifecycle
5. **Client pool** — a per-tenant PushWard API client is retrieved from an LRU pool (or created on first use) and used for all API calls
6. **State store** — PostgreSQL stores tracked state (alert instances, sync progress, download slugs) with automatic TTL expiry
7. **Two-phase end** — on completion events, handlers send a final ONGOING update (so the content appears on Dynamic Island) then ENDED after a display delay
8. **Background cleanup** — a goroutine runs every 30s to delete expired state store entries

## Authentication

### Bearer Token (most providers)

Most providers authenticate with a Bearer token containing the `hlk_` integration key in the `Authorization` header:

- Grafana
- ArgoCD
- Jellyfin
- Paperless-ngx
- Changedetection.io
- Unmanic
- Proxmox
- Overseerr
- Uptime Kuma
- Gatus
- Backrest

### HTTP Basic Auth (Radarr/Sonarr only)

Radarr and Sonarr send the `hlk_` integration key as the Basic Auth password. The username field is ignored.

