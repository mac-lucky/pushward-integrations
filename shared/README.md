[![Website](https://img.shields.io/badge/pushward.app-5B4FE5?style=for-the-badge&logo=safari&logoColor=white)](https://pushward.app)
[![App Store](https://img.shields.io/badge/App_Store-Download-0D96F6?style=for-the-badge&logo=apple&logoColor=white)](https://apps.apple.com/app/id6759689999)
[![golangci-lint](https://github.com/mac-lucky/pushward-integrations/actions/workflows/golangci-lint.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/golangci-lint.yml)
[![CI/CD Relay](https://github.com/mac-lucky/pushward-integrations/actions/workflows/relay-ci-cd.yml/badge.svg)](https://github.com/mac-lucky/pushward-integrations/actions/workflows/relay-ci-cd.yml)

# PushWard Integrations - Shared Library

Common Go building blocks every PushWard integration bridge reuses so each bridge only writes its provider-specific webhook/poll logic. It ships a hand-written [pushward-server](https://pushward.app) REST client (activities, notifications, widgets) with retry + circuit breaker, YAML+env config loading, health/ready HTTP scaffolding, a generic widget poller, fail-closed header auth, small concurrency primitives, string/slug/byte helpers, and a contract-validating mock server for tests.

This is a **library only** - no `main` package, no Dockerfile, no runnable binary. The bridges that import it (`github`, `sabnzbd`, `bambulab`, `grafana`, `relay`) are the things you build and run; see the [root README](../README.md) for those.

> **New to PushWard?** Learn what it is at **[pushward.app](https://pushward.app)** and download the iOS app on the **[App Store](https://apps.apple.com/app/id6759689999)**.

## How it works

A bridge turns an external event into a PushWard API call; this library is the layer between the bridge and the server.

```
external event -> bridge handler -> shared/pushward.Client -> pushward-server (api.pushward.app) -> APNs -> iOS Live Activity / widget
                  (provider logic)   (retry - breaker - auth)        (REST)
```

The client speaks the public pushward-server REST surface (`/activities`, `/notifications`, `/widgets`) and surfaces RFC 9457 Problem details as typed errors. Everything else in the module is supporting infrastructure the bridges share.

## Packages

| Package | Import suffix | What it provides |
|---|---|---|
| `pushward` | `.../shared/pushward` | Hand-written pushward-server REST client (activities, notifications, widgets), retry, circuit breaker, content/widget models, template/level/severity/color constants, pointer helpers, typed `HTTPError` |
| `config` | `.../shared/config` | `LoadYAML` (tolerates a missing file), `PushWardConfig` (with `DefaultPushWardConfig` for the two-phase-end defaults) / `ServerConfig` / `RenderConfig` with `PUSHWARD_*` env overrides + `Validate`, `PollingConfig` (the two-tier poll cadence: `ApplyActiveDefault` derives the active tier from the idle one, `Validate` checks both), `TimelineConfig`, the `EnvBool` / `EnvDuration` / `EnvInt` / `EnvInt64` / `EnvFloat64` override helpers |
| `server` | `.../shared/server` | `NewMux` (`/health` + `/ready` with readiness checks) and `ListenAndServe` with graceful shutdown |
| `ci` | `.../shared/ci` | The CI steps ladder: job to step-group folding (matrix legs and reusable-workflow prefixes), step colors, prior-run duration weights, live-progress anchors |
| `cipoll` | `.../shared/cipoll` | The whole poll-a-forge orchestration on top of `ci`: one activity per repo, the total-steps clamp, redundant-tick suppression, live-progress anchoring, the two-phase end. A forge plugs in through the `Forge` interface |
| `widgets` | `.../shared/widgets` | Generic background poller publishing numeric values to the widget API; `ValueSource` / `MultiValueSource` / `StatListSource` |
| `poster` | `.../shared/poster` | Activity poster images: `ValidImageURL` for the `image_url` rules, a `Source` that resolves artwork to a ThumbHash (SSRF-guarded fetch, decode-size budget, LRU cache, inline-wait budget), and `Apply` / `ApplyFetchURL` to set the trio on a `Content` |
| `auth` | `.../shared/auth` | Constant-time, fail-closed header auth middleware + inline check |
| `syncx` | `.../shared/syncx` | Small concurrency primitives: `DropCounter`, `Periodic`, `TimerGroup` |
| `text` | `.../shared/text` | `FormatBytes`, `Truncate`/`TruncateHard`, `Slug`/`SlugHash`/`HashHex`, `SanitizeURL` |
| `testutil` | `.../shared/testutil` | Contract-validating mock pushward-server for bridge tests |

## Install / Import

The module path is `github.com/mac-lucky/pushward-integrations/shared` (Go 1.26.5). It is a member of the repo-root Go workspace (`go.work` declares `use ./shared`), so the bridges import it directly - **no `replace` directive needed**.

```go
import "github.com/mac-lucky/pushward-integrations/shared/pushward"
```

Its only direct dependency is `gopkg.in/yaml.v3`; everything else is the Go standard library.

## Configuration

Bridges carry `config.PushWardConfig` and `config.ServerConfig` as named fields of their own config struct, load YAML, then apply env overrides. **Environment variables override YAML**, and the standardized prefix is `PUSHWARD_*`. The two forge bridges take `config.RenderConfig` and `config.PollingConfig` the same way and hand them to `cipoll` whole; `PollingConfig` needs an `ApplyActiveDefault()` after the last override layer, because its active tier is derived from the idle one.

```yaml
pushward:
  url: https://api.pushward.app
  api_key: YOUR_API_KEY
  priority: 5
  cleanup_delay: 15m
  stale_timeout: 30m
  end_delay: 5s
  end_display_time: 10s

server:
  address: ":8080"
  metrics_address: ":9090"
```

### `PushWardConfig` (`yaml: pushward`)

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_URL` | `pushward.url` | Base URL of the pushward-server REST API (e.g. `https://api.pushward.app`) | Yes |
| `PUSHWARD_API_KEY` | `pushward.api_key` | App API key sent as `Authorization: Bearer <key>`. Use `YOUR_API_KEY` in docs | Yes |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Default activity priority, `0`-`10` (parsed strictly with `strconv.Atoi`) | No (default `0`) |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | Go duration (e.g. `15m`); consumed by bridges for activity cleanup timing | No (default `0`) |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Go duration before an activity is considered stale without updates | No (default `0`) |
| `PUSHWARD_END_DELAY` | `pushward.end_delay` | Go duration before an activity transitions to `ended` | No (default `0`) |
| `PUSHWARD_END_DISPLAY_TIME` | `pushward.end_display_time` | Go duration the final completion frame stays visible before `ended` (two-phase end) | No (default `0`) |

`PushWardConfig.Validate()` requires `url` and `api_key` and rejects a priority outside `0`-`10`.

### Logging (`NewLogger`, env only)

`NewLogger` builds the JSON `slog` handler every bridge installs as its default, at the level named by `PUSHWARD_LOG_LEVEL` (`debug`, `info`, `warn`/`warning`, `error`; case and surrounding whitespace are ignored). It has no config key on purpose: a bridge installs the logger before it loads config, so the level is available to report a config load that fails.

Unlike the `Env*` helpers, an unrecognised value does not abort startup. It warns through the logger it just built and stays at `info` - refusing to boot over a diagnostics knob fails hardest at the moment someone is reaching for it, and the warning is the first line in the log, so the fallback is not silent.

### `ServerConfig` (`yaml: server`)

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_SERVER_ADDRESS` | `server.address` | Listen address for a bridge's webhook HTTP server (e.g. `:8080`) | No (bridge-defined) |
| `PUSHWARD_SERVER_METRICS_ADDRESS` | `server.metrics_address` | Listen address for the internal metrics server; the consuming bridge applies the `:9090` default | No |

### `TimelineConfig` (`yaml: timeline`)

Visual display settings for the `timeline` template, applied to a `pushward.Content` via `TimelineConfig.Apply`. No env overrides.

| Config Key | Type | Description | Default |
|---|---|---|---|
| `timeline.smoothing` | `*bool` | Smooth the timeline sparkline | unset |
| `timeline.scale` | `string` | Axis scale (`linear` / `logarithmic`) | `""` |
| `timeline.decimals` | `*int` | Value decimal places | unset |

## Usage

### Bootstrap pattern

```go
var cfg struct {
    PushWard config.PushWardConfig `yaml:"pushward"`
    Server   config.ServerConfig   `yaml:"server"`
}
if err := config.LoadYAML(configPath, &cfg); err != nil { // missing file is tolerated (ENOENT)
    log.Fatal(err)
}
if err := cfg.PushWard.ApplyEnvOverrides(); err != nil {
    log.Fatal(err)
}
cfg.Server.ApplyEnvOverrides()
if err := cfg.PushWard.Validate(); err != nil {
    log.Fatal(err)
}

client := pushward.NewClient(cfg.PushWard.URL, cfg.PushWard.APIKey)

// /health (liveness) + /ready (readiness checks), plus your webhook route.
// Pass any number of readiness checks; /ready returns 503 if any fails within 2s.
mux := server.NewMux(func(ctx context.Context) error { return backend.Ping(ctx) })
mux.HandleFunc("/webhook", myHandler)
server.ListenAndServe(ctx, cfg.Server.Address, mux) // graceful shutdown on ctx cancel
```

### Client

`NewClient(baseURL, apiKey string, opts ...ClientOption) *Client` - default `http.Client` timeout is 10s; auth is `Authorization: Bearer <apiKey>`. Functional options: `WithHTTPClient`, `WithOnResult` (per-call result callback), `WithCircuitBreaker`.

| Method | REST call | Notes |
|---|---|---|
| `CreateActivity(ctx, slug, name, priority, endedTTL, staleTTL)` | `POST /activities` | Upsert -> always `201`; `409` surfaced only as `activity.limit_exceeded` |
| `UpdateActivity(ctx, slug, UpdateRequest)` | `PATCH /activities/{slug}` | Full-content body: seed the session or send the final `ended` frame |
| `PatchActivity(ctx, slug, PatchRequest)` | `PATCH /activities/{slug}` | RFC 7396 merge-patch for mid-session ticks (uses `ContentPatch`) |
| `SendNotification(ctx, SendNotificationRequest)` | `POST /notifications` | Auto-fills `source_display_name` from `source` |
| `CreateWidget(ctx, CreateWidgetRequest)` | `POST /widgets` | Upsert on (user, slug); `409` = `widget.limit_exceeded` |
| `UpdateWidget(ctx, slug, UpdateWidgetRequest)` | `PATCH /widgets/{slug}` | Sends `Content-Type: application/merge-patch+json` (RFC 7396) |
| `DeleteWidget(ctx, slug)` | `DELETE /widgets/{slug}` | - |

```go
client.CreateActivity(ctx, "build-42", "CI Build #42", cfg.PushWard.Priority, 900, 1800)

// Seed (full body) with a known template/state.
client.UpdateActivity(ctx, "build-42", pushward.UpdateRequest{
    State: pushward.StateOngoing, // "ongoing"
    Content: pushward.Content{
        Template: pushward.TemplateSteps,
        Progress: 0.5,
        State:    "Building",
    },
})

// Mid-session tick (merge-patch - unset fields preserved server-side).
client.PatchActivity(ctx, "build-42", pushward.PatchRequest{
    Content: &pushward.ContentPatch{Progress: pushward.Float64Ptr(0.75)},
})
```

> Activity state constants are **lowercase**: `StateOngoing = "ongoing"`, `StateEnded = "ended"`.

**Models & constants:** `Content` (superset for full updates) vs `ContentPatch` (all-pointer, every field keeps `json:",omitempty"` per RFC 7396); template constants `TemplateGeneric` / `Alert` / `Steps` / `Countdown` / `Gauge` / `Timeline` / `Board` / `Log`; the `board` template carries `[]BoardTile` (1-4 tiles), `log` carries `[]LogLine` (1-20 lines, newest-first); trend constants `TrendUp` / `TrendDown` / `TrendFlat` (board tiles); log-level constants `LogInfo` / `LogWarn` / `LogError`; `TapAction` routing on every template/widget via `tap_action` / `url_action` / `secondary_url_action` (richer than the legacy `url` / `secondary_url` strings - adds method/headers/body for silent webhooks); notification levels `LevelActive` / `LevelPassive`; widget templates `WidgetTemplateValue` / `Progress` / `Status` / `Gauge` / `StatList` / `Trend` / `Countdown` / `Battery` / `Schedule` / `Flow`; severities `SeverityCritical` / `Warning` / `Info`; accent colors `ColorRed` / `Orange` / `Green` / `Blue` (matching iOS system colors). Helpers: `BoolPtr` / `IntPtr` / `Int64Ptr` / `Float64Ptr` / `StringPtr`, `SeverityColor` / `SeverityIcon`, `DisplayNameFor` / `(SendNotificationRequest).FillSourceDisplayName`, `MediaImage(url)`.

**Activity images:** `generic` and `steps` carry an optional poster image - `image_url` + `image_shape` (`ImageShapePoster` / `Square` / `Circle`, square when omitted) + `image_thumbhash`. The server answers **422 on every other template**, so never set them on `alert`, `countdown`, `gauge`, `timeline`, `board` or `log`. Switching *to* one of those templates clears an inherited trio server-side, so a merge-patch never has to null them; a `generic` <-> `steps` switch keeps them, because both have the slot. `image_url` must be https with a host and no userinfo (max 2048 runes). The **device** fetches it, not the server, and it also refuses private/LAN hosts - so a LAN URL is accepted by the API and never renders. `image_thumbhash` is padded standard-alphabet base64 (max 64 chars) and is the only tier that survives an unreachable URL, so for a self-hosted media server it *is* the image. Widgets have no image support. See the [`poster`](#packages) package for building all three from a provider's artwork URL.

### Poster images (`poster`)

`ValidImageURL(raw)` applies the server's syntactic `image_url` rules and nothing else - no IP filtering, because dropping a LAN URL here would also drop the thumbhash the card could still show.

`Source` resolves a URL to a thumbhash. `Disabled{}` always returns `""`, so a provider holds a `Source` unconditionally and never branches on whether the feature is on; `Static(hash)` is the canned Source for tests. `NewResolver(Config{})` builds the real one: it fetches in the background under a 2-slot semaphore, waits at most `InlineWait` (default 600ms) before answering, and caches results in a relay-wide 512-entry LRU - positives forever, failures for 15 minutes, and timeouts not at all (a deadline is a statement about load, not about the URL, and the cache is keyed by URL alone, so a poisoned CDN entry would take every tenant with it). A miss returns `""` now and lands for the next frame, which on a stream of Live Activity pushes is invisible; a webhook that blocked for the full fetch would not be. Nothing here returns an error or panics: every failure is an empty hash. `Config.OnResult` is the optional hook that reports each fetch as `ok` / `refused` / `timeout` / `error`, which is how the relay counts them without this package importing a metrics library.

Fetches are bounded on every axis a webhook payload could otherwise choose: at most 2 MiB on the wire, 8M pixels, and roughly 24 MiB of decoded frame buffer - estimated from the image header, because a 64 KB 16-bit PNG that passes a pixel cap still asks for over 100 MB, which is more than a relay pod is allowed. URLs longer than `MaxImageURLRunes` are refused before they can become cache keys, and userinfo is refused outright (Go forwards `user:pass@` as an `Authorization` header).

`AllowPrivateHosts` is **off by default**. The hosted relay is multi-tenant and its webhook payloads are attacker-controlled, so with it off the resolver fetches `https` only, and the dialer refuses loopback, RFC 1918, CGNAT/Tailscale, link-local, multicast and the other special-purpose ranges - including every IPv6 spelling of a v4 address (v4-mapped, v4-compatible, NAT64, 6to4), checked on the *resolved* address per dial, which is what makes it DNS-rebinding safe and what catches a public host redirecting to a private one. A self-hosted relay that should reach a LAN media server turns it on, which also re-allows cleartext `http`.

`Apply(ctx, src, &content, url, shape)` sets all three fields at once, writing the shape only when a URL or hash survived. A `Disabled` source writes none of them: off has to mean the card carries no image fields, since `image_url` alone would still publish the media server's hostname. `ApplyFetchURL` is the variant for a provider whose fetch URL differs from what the device should render (Jellyfin asks for a small transcoded JPEG).

The encoder is a vendored, encode-only port of [ThumbHash](https://github.com/evanw/thumbhash) (MIT), decoding through the standard library only - `image/jpeg`, `image/png`, `image/gif`. **WebP and AVIF do not decode and yield no hash**, deliberately: no new module dependency is worth a placeholder, and a new `require` in `shared/go.mod` ripples `go.sum` churn into all seven modules.

### Resilience

Every call goes through one retry/backoff path (`doWithRetry`):

- **Up to 5 attempts.** Integer exponential backoff with equal jitter, capped at 30s, on `5xx` / network errors.
- **`429`** honors the `Retry-After` header or `problem.retry_after_ms`, clamped to a 2-minute maximum so a hostile/buggy value can't park the goroutine.
- **Non-`409` `4xx`** fails fast (no retry).
- **`409`** is handled per call - `activity.limit_exceeded` / `widget.limit_exceeded` surface as typed errors.

**Error model:** failures return `*HTTPError` carrying `StatusCode` plus RFC 9457 Problem fields (`Type` / `Title` / `Detail` / `Code` / `RetryAfterMs`). Branch on the stable `Code` via `errors.As`:

```go
var he *pushward.HTTPError
if errors.As(err, &he) && he.Code == pushward.ErrCodeActivityLimitExceeded {
    // back off creating new activities for this app
}
```

**Circuit breaker (optional):** `NewCircuitBreaker(threshold, cooldown)` opens after N consecutive backend faults and half-opens for a single probe after the cooldown. Only genuine `5xx`/network exhaustion trips it; `4xx`/`409`/`429` prove the backend is reachable and never open it. Attach with `WithCircuitBreaker`; calls return `ErrCircuitOpen` while open. The relay shares one breaker across all tenants.

### Widget poller (`widgets`)

A generic background poller that publishes numeric values to the widget API - a primitive separate from Live Activities. `New(client, []Spec, logger)` validates specs; `Manager.Start` spawns one goroutine per scalar widget and one supervisor per multi-source / `stat_list` spec. The first poll is synchronous so the initial `CreateWidget` carries a real value; `gauge`/`progress` defer creation until the first numeric value. Implement `ValueSource` (single scalar), `MultiValueSource` (label-keyed fan-out), or `StatListSource` (rows), or use the `...Func` adapters. Return `ErrNoData` to skip a tick.

Defaults: `DefaultInterval` 60s, `DefaultMaxSeries` 20, `DefaultMaxStatRows` 6 (server cap), `DefaultMissGrace` 2; ticker jitter is 25% of the interval. `UpdateMode` is `UpdateOnChange` (default, within `MinChange` tolerance) or `UpdateAlways`. `Spec.PushThrottle` and `Spec.StaleAfter` are passed through at create time only - the manager's PATCHes carry content, never config, so changing either needs the widget recreated. Currently consumed by the `grafana` bridge.

The manager drives the five scalar-shaped templates. `trend` also defers creation until a value exists, but its `points` array has no source behind it: seed `Spec.Content.Points` yourself or the create is rejected. `battery`, `schedule` and `flow` need whole objects per tick rather than a number, so they are modelled in `pushward` (`BatteryDevice`, `SchedulePeriod`, `FlowNode` / `WidgetFlow`, plus `TimerValue` for `subtitle_timer` and `stat_rows[].timer`) and posted directly through the client.

### Auth (`auth`)

`RequireHeader(header, expected)` middleware and `CheckHeader(r, header, expected)` inline check, both constant-time (`crypto/subtle`) and **fail-closed**: an empty `expected` rejects/returns false for every request, so an unconfigured secret never authenticates.

### Concurrency primitives (`syncx`)

| Type | Purpose |
|---|---|
| `DropCounter` | Counts drops; logs on the 1st and every Nth drop |
| `Periodic` | Runs a function on an interval in a background goroutine (`Start` / `Stop`) |
| `TimerGroup` | Single re-armable `AfterFunc` timer with `Reset` / `Stop` / `Close` / `Wait` - drives the relay's two-phase end |

All are zero-value-or-constructor ready and concurrency-safe.

### Text helpers (`text`)

`FormatBytes(int64)` -> human-readable size; `Truncate` / `TruncateHard` (rune-safe); `Slug(prefix, input)` URL-safe slug with a SHA-256 fallback for non-alphanumeric input; `SlugHash` / `HashHex`; `SanitizeURL` (returns the input only if it is `http`/`https` with a host); the exported `NonAlphanumeric` regexp.

### Test utilities (`testutil`)

`MockPushWardServer(t)` starts an `httptest` server that records calls **and validates them against the public API contract** (slug pattern, name/field length caps, per-template required fields, color/URL rules, the generic/steps-only activity-image trio), returning proper `201`/`200`/`400`/`404` with RFC 9457 Problem bodies - not a blind `200 OK`. Also `MockPushWardServerFailingPatches` (drives update-failure paths), `GetCalls`, `CountPath`, `UnmarshalBody`, `RequireValueMap`, and the `APICall` struct.

## Development

Run from the `shared/` directory:

```bash
go build ./...
go vet ./...
go test ./... -count=1
```

Or from the repo root (the Go workspace):

```bash
go test ./shared/... -v -count=1
```

Lint matches CI (`golangci-lint` `v2.11.4`, run at the workspace root):

```bash
golangci-lint run
```

Every bridge CI runs its tests with the race detector (`-race -count=1 -v`); add `-race` locally when touching shared code those bridges exercise.

## CI/CD

`shared/` produces **no image and no binary** - it has no release of its own. Because every bridge imports it, a change under `shared/**` triggers each bridge's CI workflow (via path filters) plus the workspace-wide `golangci-lint` workflow, and ships only when a consuming bridge is tagged `<bridge>/v<X.Y.Z>`. Be conservative about churn here: one shared edit re-tests and can re-release every bridge. Release mechanics live in the [root README](../README.md).

## Server compatibility

The `pushward.Client` is **hand-written, not generated** - keep it in sync with the contract owner, [pushward-server](https://pushward.app)'s `openapi.yaml` (endpoints, request/response JSON keys & casing, auth headers, RFC 9457 Problem codes). Note the casing split that is the historical #1 bug source: REST bodies are snake_case, while the activity content model mirrors the iOS APNs `ContentState`. Released App Store iOS clients **cannot be hot-fixed**, so never remove/rename a route, change key casing, or tighten a required field without confirming backward compatibility first.

## Troubleshooting

This module logs via the standard library `log/slog`; the consuming bridge configures the handler and level. Enable debug output in the bridge (e.g. set its log level to `debug`) to see retry/backoff and rate-limit warnings.

| Symptom | Cause / fix |
|---|---|
| `pushward.url is required` / `pushward.api_key is required` at startup | `Validate()` failed - set `url` and `api_key` (or `PUSHWARD_URL` / `PUSHWARD_API_KEY`). Remember env vars override YAML. |
| `pushward.priority must be 0-10` | Priority out of range; or `PUSHWARD_PRIORITY` had trailing garbage (parsed strictly). |
| Repeated `retrying PushWard request` warnings | Backend returning `5xx` / network errors; the client retries up to 5x with capped backoff. Check server health and the `url`. |
| `rate limited by PushWard` warnings | `429` responses; the client honors `Retry-After` (clamped to 2 min). Reduce send rate. |
| `*HTTPError` with `Code == activity.limit_exceeded` / `widget.limit_exceeded` | The app hit its activity/widget cap on the server - stop creating new ones, or raise the plan limit. |
| `ErrCircuitOpen` | The attached circuit breaker is open after sustained backend faults; it half-opens after the cooldown. |
| `401 unauthorized` from a bridge's own webhook route | `auth` is fail-closed - an empty expected secret rejects every request. Configure the webhook token. |
| `widgets` widget never created | A `gauge`/`progress` source returned no numeric value yet, or the source returns `ErrNoData` every tick. |

## Requirements

- **Go 1.26.5** (see `go.mod`); part of the repo-root `go.work` workspace.
- A running [pushward-server](https://pushward.app) and an app API key (the consuming bridge supplies these).

## License

Part of the public [`pushward-integrations`](https://github.com/mac-lucky/pushward-integrations) repository. See the repository for license terms.
