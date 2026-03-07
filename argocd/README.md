# pushward-argocd

Bridges ArgoCD sync notifications to [PushWard](https://github.com/mac-lucky/pushward-server) Live Activities on iOS.

Receives webhook events from argocd-notifications and maps sync progress to a 3-step pipeline Live Activity: **Syncing -> Rolling Out -> Deployed**. Each step appears on your iPhone's Dynamic Island and Lock Screen with real-time progress updates.

## Features

- **3-step pipeline tracking** — maps ArgoCD sync lifecycle to Syncing (1/3), Rolling Out (2/3), and Deployed (3/3)
- **Sync grace period** — defers activity creation for fast no-op syncs (default 10s), preventing unnecessary notifications for auto-syncs that complete instantly
- **Transient health-degraded handling** — during rollout (step 2), a `health-degraded` event shows a warning icon without ending the activity, allowing `deployed` to complete normally
- **Two-phase end** — sends ONGOING with final content first, then ENDED after a display delay, ensuring the Dynamic Island shows the completion state before dismissal
- **ArgoCD URL links** — activities include a link to the ArgoCD UI and a commit link derived from the repo URL and revision
- **Webhook secret validation** — optional `X-Webhook-Secret` header validation for secure webhook delivery
- **Recent deploys tracking** — detects out-of-order events after bridge restarts by recording recent deploys
- **Auto-activity management** — creates the PushWard activity on first webhook, no manual setup needed
- **Retry with backoff** — PushWard API calls retry up to 5 times with exponential backoff
- **Graceful shutdown** — waits for in-flight requests on SIGINT/SIGTERM

## Prerequisites

- A running PushWard server
- A PushWard integration key (`hlk_` prefix) with `activity:manage` scope
- An ArgoCD instance with argocd-notifications configured
- The PushWard iOS app subscribed to the activity

## Configuration

All settings can be provided via YAML config file or environment variables. Environment variables take precedence.

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_URL` | `pushward.url` | PushWard server URL | Yes |
| `PUSHWARD_API_KEY` | `pushward.api_key` | PushWard integration key (`hlk_`) | Yes |
| `PUSHWARD_ARGOCD_WEBHOOK_SECRET` | `argocd.webhook_secret` | Webhook secret for `X-Webhook-Secret` header validation | No |
| `PUSHWARD_ARGOCD_URL` | `argocd.url` | ArgoCD UI base URL (e.g. `https://argocd.example.com`) — enables "View in ArgoCD" link | No |
| `PUSHWARD_SYNC_GRACE_PERIOD` | `pushward.sync_grace_period` | Skip no-op syncs completing within this window (default: `10s`, `0` to disable) | No |
| `PUSHWARD_SERVER_ADDRESS` | `server.address` | HTTP listen address (default: `:8090`) | No |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Activity priority 0-10 (default: `3`) | No |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | Server-side `ended_ttl` for auto-deleting ended activities (default: `5m`) | No |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Server-side `stale_ttl` for auto-ending stale activities (default: `30m`) | No |
| `PUSHWARD_END_DELAY` | `pushward.end_delay` | Wait before sending ONGOING with final content in two-phase end (default: `5s`) | No |
| `PUSHWARD_END_DISPLAY_TIME` | `pushward.end_display_time` | Display time before sending ENDED in two-phase end (default: `4s`) | No |

## Docker Compose

```yaml
services:
  pushward-argocd:
    image: ghcr.io/mac-lucky/pushward-argocd:latest
    ports:
      - "8090:8090"
    volumes:
      - ./config.yml:/config/config.yml:ro
    environment:
      - PUSHWARD_URL=https://api.pushward.app
      - PUSHWARD_API_KEY=hlk_xxxxxxxxxxxx
      - PUSHWARD_ARGOCD_URL=https://argocd.example.com
```

## ArgoCD Notifications Setup

pushward-argocd receives webhooks from [argocd-notifications](https://argo-cd.readthedocs.io/en/stable/operator-manual/notifications/). Add the following to your `argocd-notifications-cm` ConfigMap:

### 1. Webhook service

```yaml
service.webhook.pushward: |
  url: http://pushward-argocd.pushward:8090
  headers:
    - name: Content-Type
      value: application/json
    - name: X-Webhook-Secret
      value: $webhook-secret    # optional, from argocd-notifications-secret
```

### 2. Templates

Create one template per event type:

```yaml
template.pushward-sync-running: |
  webhook:
    pushward:
      method: POST
      path: /webhook
      body: |
        {
          "app": "{{.app.metadata.name}}",
          "event": "sync-running",
          "revision": "{{.app.status.operationState.syncResult.revision}}",
          "repo_url": "{{.app.spec.source.repoURL}}"
        }

template.pushward-sync-succeeded: |
  webhook:
    pushward:
      method: POST
      path: /webhook
      body: |
        {
          "app": "{{.app.metadata.name}}",
          "event": "sync-succeeded",
          "revision": "{{.app.status.operationState.syncResult.revision}}",
          "repo_url": "{{.app.spec.source.repoURL}}"
        }

template.pushward-sync-failed: |
  webhook:
    pushward:
      method: POST
      path: /webhook
      body: |
        {
          "app": "{{.app.metadata.name}}",
          "event": "sync-failed",
          "revision": "{{.app.status.operationState.syncResult.revision}}",
          "repo_url": "{{.app.spec.source.repoURL}}"
        }

template.pushward-deployed: |
  webhook:
    pushward:
      method: POST
      path: /webhook
      body: |
        {
          "app": "{{.app.metadata.name}}",
          "event": "deployed",
          "revision": "{{.app.status.sync.revision}}",
          "repo_url": "{{.app.spec.source.repoURL}}"
        }

template.pushward-health-degraded: |
  webhook:
    pushward:
      method: POST
      path: /webhook
      body: |
        {
          "app": "{{.app.metadata.name}}",
          "event": "health-degraded",
          "revision": "{{.app.status.sync.revision}}",
          "repo_url": "{{.app.spec.source.repoURL}}"
        }
```

### 3. Triggers

```yaml
trigger.on-pushward-sync-running: |
  - when: app.status.operationState.phase in ['Running']
    oncePer: app.status.operationState.syncResult.revision
    send: [pushward-sync-running]

trigger.on-pushward-sync-succeeded: |
  - when: app.status.operationState.phase in ['Succeeded']
    oncePer: app.status.operationState.syncResult.revision
    send: [pushward-sync-succeeded]

trigger.on-pushward-sync-failed: |
  - when: app.status.operationState.phase in ['Error', 'Failed']
    oncePer: app.status.operationState.syncResult.revision
    send: [pushward-sync-failed]

trigger.on-pushward-deployed: |
  - when: app.status.health.status == 'Healthy' and app.status.sync.status == 'Synced'
    oncePer: app.status.sync.revision
    send: [pushward-deployed]

trigger.on-pushward-health-degraded: |
  - when: app.status.health.status == 'Degraded'
    oncePer: app.status.sync.revision
    send: [pushward-health-degraded]
```

### 4. Subscribe

Per-app (annotations on the Application resource):

```yaml
notifications.argoproj.io/subscribe.on-pushward-sync-running.pushward: ""
notifications.argoproj.io/subscribe.on-pushward-sync-succeeded.pushward: ""
notifications.argoproj.io/subscribe.on-pushward-sync-failed.pushward: ""
notifications.argoproj.io/subscribe.on-pushward-deployed.pushward: ""
notifications.argoproj.io/subscribe.on-pushward-health-degraded.pushward: ""
```

Or globally via `defaultTriggers` in `argocd-notifications-cm`.

## Endpoints

| Method | Path | Description |
|---|---|---|
| POST | `/webhook` | Receives ArgoCD notification webhooks |
| GET | `/health` | Health check (returns `ok`) |

## Sync Lifecycle

| ArgoCD Event | Step | State Text | PushWard State | Color |
|---|---|---|---|---|
| `sync-running` | 1/3 | Syncing... | ONGOING | `#007AFF` (blue) |
| `sync-succeeded` | 2/3 | Rolling out... | ONGOING | `#007AFF` (blue) |
| `deployed` | 3/3 | Deployed | ENDED | `#34C759` (green) |
| `sync-failed` | current | Sync Failed | ENDED | `#FF3B30` (red) |
| `health-degraded` | current | Degraded | ENDED | `#FF9500` (orange) |

> When `health-degraded` arrives during step 2 on a tracked app, it sends an ONGOING update with an orange warning icon instead of ending — allowing `deployed` to complete the activity normally.

## How It Works

1. **Webhook** — ArgoCD notifications sends a POST to `/webhook` when sync events occur
2. **Grace period** — activity creation is deferred for the configured grace period (default 10s). Syncs that complete within this window (e.g. no-op auto-syncs) are silently skipped
3. **Activity creation** — once the grace period expires, a PushWard activity is auto-created with the `pipeline` content template
4. **Progress tracking** — each sync event advances the pipeline step (1/3 Syncing, 2/3 Rolling out, 3/3 Deployed)
5. **Two-phase end** — on completion or failure, sends an ONGOING update with final content (e.g. "Deployed" with green checkmark), waits for the display time, then sends ENDED to dismiss the Live Activity
6. **Stale timeout** — if a sync receives no updates for 30m, the server auto-ends it via `stale_ttl`

### Activity slug format

`argocd-<sanitized-app-name>` (e.g. `argocd-pushward-server`). Non-alphanumeric characters are replaced with hyphens.
