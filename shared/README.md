# shared

Common Go libraries used by all PushWard integrations. This module is part of the [Go workspace](https://go.dev/doc/tutorial/workspaces) defined in the repository root, so integrations import it directly without a replace directive.

```go
import "github.com/mac-lucky/pushward-integrations/shared/pushward"
```

## Packages

### `pushward` — API Client

HTTP client for the PushWard server API with built-in retry logic.

**Client:**

| Function | Description |
|----------|-------------|
| `NewClient(baseURL, apiKey string) *Client` | Create a client (10s HTTP timeout, Bearer auth) |
| `CreateActivity(ctx, slug, name string, priority, endedTTL, staleTTL int) error` | `POST /activities` — server upserts and always returns 201; 409 is surfaced only for `activity.limit_exceeded` |
| `UpdateActivity(ctx, slug string, req UpdateRequest) error` | `PATCH /activities/{slug}` |

Retry behavior (up to 5 attempts):
- **5xx / network errors** — exponential backoff with jitter (capped at 30s)
- **429** — respects `Retry-After` header (seconds or HTTP-date)
- **4xx (non-409)** — fails immediately, no retry

**Types:**

| Type | Description |
|------|-------------|
| `Content` | Superset of all content fields across integrations (template, progress, state, icon, steps, URLs, severity, etc.) |
| `CreateActivityRequest` | Body for `POST /activities` (slug, name, priority, ended_ttl, stale_ttl) |
| `UpdateRequest` | Body for `PATCH /activities/{slug}` (state + content) |
| `StateOngoing` / `StateEnded` | Activity state constants (`"ONGOING"`, `"ENDED"`) |
| `IntPtr(v int) *int` | Helper for optional int fields |
| `Int64Ptr(v int64) *int64` | Helper for optional int64 fields |

### `config` — Configuration Loading

YAML config file loading with environment variable overrides.

| Type / Function | Description |
|-----------------|-------------|
| `LoadYAML(path string, target any) error` | Unmarshal a YAML file into any struct. Missing files are silently tolerated (ENOENT). |
| `PushWardConfig` | Common API settings: `url`, `api_key`, `priority` (0-10), `cleanup_delay`, `stale_timeout`, `end_delay`, `end_display_time` |
| `PushWardConfig.ApplyEnvOverrides() error` | Override fields from `PUSHWARD_URL`, `PUSHWARD_API_KEY`, `PUSHWARD_PRIORITY`, `PUSHWARD_CLEANUP_DELAY`, `PUSHWARD_STALE_TIMEOUT`, `PUSHWARD_END_DELAY`, `PUSHWARD_END_DISPLAY_TIME` |
| `PushWardConfig.Validate() error` | Require `url` and `api_key`; enforce priority range 0-10 |
| `ServerConfig` | HTTP server settings: `address` |
| `ServerConfig.ApplyEnvOverrides()` | Override from `PUSHWARD_SERVER_ADDRESS` |

### `server` — HTTP Server

Preconfigured HTTP server with health check and graceful shutdown.

| Function | Description |
|----------|-------------|
| `NewMux() *http.ServeMux` | Returns a mux with `GET /health` already registered |
| `ListenAndServe(ctx, addr string, handler http.Handler) error` | Starts the server, blocks until ctx is cancelled, then gracefully shuts down (5s timeout). Configures read/write/idle timeouts. |

### `text` — String Utilities

Text manipulation helpers for building activity content.

| Function | Description |
|----------|-------------|
| `Truncate(s string, maxLen int) string` | Truncate to maxLen runes, appending `"..."` if truncated (Unicode-safe) |
| `TruncateHard(s string, maxLen int) string` | Truncate to maxLen runes without suffix |
| `Slug(prefix, input string) string` | Generate a URL-safe slug: lowercase, non-alphanumeric runs replaced with hyphens |
| `SlugHash(prefix, input string, hashBytes int) string` | Generate a `<prefix>-<hex>` slug using SHA-256 of input |
| `SanitizeURL(rawURL string) string` | Return the URL if it has an http/https scheme, empty string otherwise |
| `NonAlphanumeric` | Compiled regexp matching `[^a-z0-9]+` |

### `testutil` — Test Helpers

Utilities for integration tests that verify PushWard API calls.

| Type / Function | Description |
|-----------------|-------------|
| `APICall` | Recorded request: Method, Path, Body (`json.RawMessage`) |
| `MockPushWardServer(t) (*httptest.Server, *[]APICall, *sync.Mutex)` | Start a mock server that records all requests and responds 200 OK |
| `GetCalls(calls, mu) []APICall` | Thread-safe snapshot of recorded calls |
| `UnmarshalBody(t, raw, v)` | Decode a recorded call's JSON body into any struct |

## Usage

Typical integration bootstrap pattern:

```go
// Load config
var cfg struct {
    PushWard config.PushWardConfig `yaml:"pushward"`
    Server   config.ServerConfig   `yaml:"server"`
}
config.LoadYAML(configPath, &cfg)
cfg.PushWard.ApplyEnvOverrides()
cfg.Server.ApplyEnvOverrides()
cfg.PushWard.Validate()

// Create API client
client := pushward.NewClient(cfg.PushWard.URL, cfg.PushWard.APIKey)

// Create and update activities
client.CreateActivity(ctx, "my-slug", "My Activity", 3, 900, 1800)
client.UpdateActivity(ctx, "my-slug", pushward.UpdateRequest{
    State: pushward.StateOngoing,
    Content: pushward.Content{
        Template: "steps",
        Progress: 0.5,
        State:    "Building",
    },
})

// Start HTTP server with health check
mux := server.NewMux()
mux.HandleFunc("/webhook", myHandler)
server.ListenAndServe(ctx, cfg.Server.Address, mux)
```

## Testing

```bash
go test ./shared/... -v -count=1
```
