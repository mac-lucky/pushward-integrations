[![Website](https://img.shields.io/badge/pushward.app-5B4FE5?style=for-the-badge&logo=safari&logoColor=white)](https://pushward.app)
[![App Store](https://img.shields.io/badge/App_Store-Download-0D96F6?style=for-the-badge&logo=apple&logoColor=white)](https://apps.apple.com/app/id6759689999)

# PushWard for Forgejo Actions

Turns a Forgejo Actions run into a live **PushWard Live Activity**: a step ladder on the Lock
Screen and Dynamic Island that fills in as each job group finishes, and clears itself a few
seconds after the run ends.

## How it works

The bridge polls your Forgejo instance's Actions API. When a repo has a run in progress it
creates one Live Activity for that repo, then updates it as jobs move through the DAG. Jobs are
folded into **step groups** by name, so a matrix like `tofu (tailscale)` / `tofu (grafana)` /
`tofu (cloudflare)` renders as one `tofu` step three rows wide.

```
Forgejo Actions --> pushward-forgejo --> pushward-server --> APNs --> PushWard iOS app
   (polled)           steps ladder         PATCH /activities
```

## Features

- **Stable denominator from the first frame.** A fresh run has only revealed its first wave of
  jobs, so the step count would otherwise climb (1/2 -> 3/4 -> 5/6). The bridge seeds the shape
  from the last successful run of the same workflow and branch, which already ran the whole DAG.
- **Duration-sized step pills** (`step_weights`, opt-in). Each group's pill is sized by how long
  it ran last time.
- **A live ETA on the running step** (`live_progress`, on by default). iOS fills the current
  pill and counts it down between polls, anchored to when the step actually started.
- **Color-coded steps** (`step_colors`, opt-in): tests one hue, build another, deploy another.
- **Two-phase end.** The result lands as a final ongoing frame so it is visible on the Dynamic
  Island, then the activity is dismissed a few seconds later.
- **Cheap when idle.** With nothing running, the per-repo poll is a few dozen bytes. Detection and live updates are separate intervals, so a smoother card does not mean polling every repo more often.

## Prerequisites

- A Forgejo instance with Actions enabled, reachable from wherever this runs.
- A Forgejo **API token** with read access to the repos you want to watch. It does *not* need the
  `read:organization` scope - discovery falls back to the user endpoint when that scope is absent.
- A **PushWard integration key** (`hlk_` prefix) and the PushWard iOS app.

## Installation

### Docker run

```bash
docker run -d --name pushward-forgejo \
  -e PUSHWARD_FORGEJO_URL="https://git.example.com" \
  -e PUSHWARD_FORGEJO_TOKEN="..." \
  -e PUSHWARD_FORGEJO_OWNER="your-user-or-org" \
  -e PUSHWARD_API_KEY="hlk_..." \
  ghcr.io/mac-lucky/pushward-forgejo:latest
```

### Docker Compose

```yaml
services:
  pushward-forgejo:
    image: ghcr.io/mac-lucky/pushward-forgejo:latest
    container_name: pushward-forgejo
    restart: unless-stopped
    environment:
      PUSHWARD_FORGEJO_URL: "https://git.example.com"
      PUSHWARD_FORGEJO_TOKEN: "${FORGEJO_TOKEN}"
      PUSHWARD_FORGEJO_OWNER: "your-user-or-org"
      PUSHWARD_API_KEY: "${PUSHWARD_API_KEY}"
      PUSHWARD_FORGEJO_STEP_COLORS: "true"
      PUSHWARD_FORGEJO_STEP_WEIGHTS: "true"
```

Mount a config file at `/config/config.yml` instead if you prefer; start from
[`config.example.yml`](./config.example.yml). A missing file is tolerated, so an env-only
deployment works with no file at all.

## Configuration

Environment variables always win over the YAML file.

| Env | YAML | What it does | Default |
|---|---|---|---|
| `PUSHWARD_FORGEJO_URL` | `forgejo.url` | Instance root, e.g. `https://git.example.com`. `/api/v1` is appended for you; passing it yourself is also accepted. **Required** | - |
| `PUSHWARD_FORGEJO_TOKEN` | `forgejo.token` | Forgejo API token. **Required** | - |
| `PUSHWARD_FORGEJO_OWNER` | `forgejo.owner` | Auto-discovers repos. When it matches the token's own login, every repo the token can reach is discovered - including repos owned by others | - |
| `PUSHWARD_FORGEJO_REPOS` | `forgejo.repos` | Comma-separated `owner/repo` list, watched in addition to whatever `owner` discovers | - |
| `PUSHWARD_FORGEJO_TIMEOUT` | `forgejo.timeout` | Bounds one API call | `15s` |
| `PUSHWARD_POLL_IDLE` | `polling.idle_interval` | How often each watched repo is checked for a run that has just started - one request per repo per pass | `60s` |
| `PUSHWARD_POLL_INTERVAL` | `polling.interval` | How often a run already in flight is advanced - one request per running run. Must not exceed `idle_interval` | smaller of `idle_interval` and `15s` |
| `PUSHWARD_FORGEJO_STEP_COLORS` | `render.step_colors` | Tint step pills by job type | `false` |
| `PUSHWARD_FORGEJO_STEP_WEIGHTS` | `render.step_weights` | Size step pills by prior-run duration | `false` |
| `PUSHWARD_FORGEJO_LIVE_PROGRESS` | `render.live_progress` | Fill the running step and count its ETA down | `true` |

One of `forgejo.owner` or `forgejo.repos` is required. The shared `pushward.*` block (`url`,
`api_key`, `priority`, `cleanup_delay`, `stale_timeout`, `end_delay`, `end_display_time`) is
documented in the [root README](../#configuration).

## How it maps to a Live Activity

| Live Activity field | Source |
|---|---|
| Slug | `fj-<sha256(owner/repo)[:8]>` - one activity per repo |
| Name | `Forgejo: <repo>` |
| Template | `steps` |
| Subtitle | `<repo> / <workflow>`, where the workflow is its filename without the extension |
| Step groups | Job names with matrix parameters and any `caller / ` prefix stripped |
| Progress | Completed jobs over total jobs |
| Link | The run's own `html_url` |
| Final state | `Success` / `Failed` / `Cancelled` / `Skipped`, from the run's status |

## Running alongside the relay

The [relay](../relay/) also has a `/forgejo` route. It receives Forgejo's terminal
`action_run_success` / `action_run_failure` / `action_run_recover` webhooks and shows a single
completion result; this bridge polls the API and shows live per-job progress.

They use different slug prefixes (`forgejo-` vs `fj-`), so they will never fight over one
activity - but pointed at the same account, each build produces **two** cards. Pick one: either
remove the Forgejo webhook, or set `providers.gitea.enabled: false` on the relay if you use it
for nothing else.

## Development

```bash
go build ./cmd/pushward-forgejo
go test ./... -race -count=1

# Lint per module. A root-level run on this go.work repo is a false pass.
golangci-lint run ./...

# Docker: the build context is the REPO ROOT so the Dockerfile can COPY shared/.
docker build -f forgejo/Dockerfile -t pushward-forgejo ..
```

## Forgejo API notes

Verified against Forgejo **16.0.1+gitea-1.22.0**. These are the places the API differs from
GitHub's in ways that matter, and they are all covered by tests and by the fixtures in
[`testdata/`](./testdata/):

- A run's `html_url` is built from `index_in_repo`, **not** the `id` the API is addressed by, so
  the bridge always uses the URL the API hands it.
- There is no `conclusion` field. One `status` enum carries everything, and the
  status/conclusion pair the steps ladder wants is synthesised at the client boundary.
- Job objects carry no timestamps at all. Per-job timing comes from `/actions/tasks`, joined on
  `task_id`. When that join misses, the step pills fall back to equal width and the ETA is simply
  not shown - nothing breaks.
- `workflow_id` is a filename string, and there is no workflow display name anywhere in the API.
- Runs held for approval (`blocked`) are not tracked: they may never execute.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| No activity ever appears | Token cannot see the repo, or `owner`/`repos` names a repo that does not exist. Startup logs the instance version and the resolved repo list |
| Steps show as `1/1` | The bridge could not read the run's jobs; check the token's repo access |
| Pills are all the same width | `step_weights` is off, or no prior successful run of that workflow and branch exists yet |
| No ETA countdown | Expected on the first run of a workflow - there is nothing measured to count down from |
| Two cards per build | The relay's `/forgejo` webhook is configured as well; see above |
| Connection timeouts | The instance is not reachable from the container's network |

## License

MIT, same as the rest of this repository.
