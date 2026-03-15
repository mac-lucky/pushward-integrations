# pushward-mqtt

A generic MQTT-to-Live-Activity bridge for [PushWard](https://pushward.app) on iOS.

Subscribes to any MQTT broker, matches messages against configurable rules, and maps JSON payloads to Live Activities on your iPhone's Dynamic Island and Lock Screen. Works with Zigbee2MQTT, Home Assistant, custom sensors, or any MQTT source that publishes JSON.

## Features

- **Configurable rules** — define topics, field mappings, icons, and colors in YAML without writing code
- **Two lifecycle modes** — `field` (state transitions driven by a JSON field value) and `presence` (activity exists while messages arrive)
- **Template strings** — `{field | transform}` syntax with pipe transforms for formatting, math, and scaling
- **Conditional mapping** — dynamic icon and accent color based on field values
- **MQTT wildcards** — `+` (single level) and `#` (multi level) topic matching
- **Virtual fields** — `_topic` and `_topic_segment:N` available in templates for dynamic slugs and content
- **Dynamic slugs** — `{field}` substitution in slug names for per-device activities
- **Debounced updates** — configurable interval to avoid flooding the PushWard API
- **Two-phase end** — shows final state on Dynamic Island before dismissing
- **Configurable TLS** — CA cert, client cert/key, and insecure skip verify options
- **Auto-reconnect** — re-subscribes to all topics on MQTT reconnect
- **Retry with backoff** — PushWard API calls retry up to 5 times with exponential backoff
- **Graceful shutdown** — stops all trackers and cancels pending timers on SIGINT/SIGTERM

## Prerequisites

- A running PushWard server
- A PushWard integration key (`hlk_` prefix)
- An MQTT broker accessible from the bridge
- The PushWard iOS app subscribed to the activity

## Configuration

Connection and PushWard settings can be provided via YAML config file or environment variables. Environment variables take precedence. Rules must be defined in the YAML config file.

| Env Variable | Config Key | Description | Required |
|---|---|---|---|
| `PUSHWARD_MQTT_BROKER` | `mqtt.broker` | MQTT broker URL (e.g. `tcp://192.168.1.50:1883`) | Yes |
| `PUSHWARD_MQTT_USERNAME` | `mqtt.username` | MQTT username | No |
| `PUSHWARD_MQTT_PASSWORD` | `mqtt.password` | MQTT password | No |
| `PUSHWARD_MQTT_CLIENT_ID` | `mqtt.client_id` | MQTT client ID (default: `pushward-mqtt`) | No |
| `PUSHWARD_URL` | `pushward.url` | PushWard server URL | Yes |
| `PUSHWARD_API_KEY` | `pushward.api_key` | PushWard integration key (`hlk_`) | Yes |
| `PUSHWARD_POLL_INTERVAL` | `polling.update_interval` | Debounce interval for updates (default: `5s`) | No |
| `PUSHWARD_PRIORITY` | `pushward.priority` | Default activity priority 0-10 (default: `1`) | No |
| `PUSHWARD_CLEANUP_DELAY` | `pushward.cleanup_delay` | Server-side `ended_ttl` for ended activities (default: `15m`) | No |
| `PUSHWARD_STALE_TIMEOUT` | `pushward.stale_timeout` | Server-side `stale_ttl` for stuck activities (default: `30m`) | No |
| `PUSHWARD_END_DELAY` | `pushward.end_delay` | Delay before phase 1 of two-phase end (default: `5s`) | No |
| `PUSHWARD_END_DISPLAY_TIME` | `pushward.end_display_time` | Display time before sending ENDED (default: `4s`) | No |

> **Note:** Rules (topic subscriptions, field mappings, lifecycle modes) cannot be set via environment variables — they must be defined in the YAML config file.

## Docker Compose

```yaml
services:
  pushward-mqtt:
    image: ghcr.io/mac-lucky/pushward-mqtt:latest
    volumes:
      - ./config.yml:/config/config.yml:ro
    environment:
      - PUSHWARD_URL=https://api.pushward.app
      - PUSHWARD_API_KEY=hlk_xxxxxxxxxxxx
      - PUSHWARD_MQTT_BROKER=tcp://192.168.1.50:1883
```

> No ports are exposed — the bridge connects outbound to the MQTT broker and to the PushWard API via HTTPS.

## Rule Configuration

Each rule maps an MQTT topic to a PushWard Live Activity. Rules are defined in the `rules` array in the YAML config file.

### Field lifecycle

State transitions are driven by a JSON field value. Use `state_map` to map field values to `ONGOING`, `ENDED`, or `IGNORE`.

```yaml
rules:
  - name: "Washing Machine"
    topic: "zigbee2mqtt/washing_machine"
    slug: "washer"
    template: "generic"
    priority: 2
    lifecycle: "field"
    state_field: "running_state"
    state_map:
      idle: "IGNORE"
      running: "ONGOING"
      paused: "ONGOING"
      done: "ENDED"
    content:
      state: "{running_state}"
      subtitle: "{program_name}"
      progress: "{completion_percentage | div:100}"
      icon:
        default: "washer.fill"
        map:
          running_state:
            paused: "pause.circle.fill"
      accent_color:
        default: "blue"
        map:
          running_state:
            paused: "orange"
            done: "green"
      remaining_time: "{remaining_minutes | mul:60}"
```

| Field | Description | Required |
|---|---|---|
| `name` | Human-readable rule name (used as activity name) | Yes |
| `topic` | MQTT topic to subscribe to (supports `+` and `#` wildcards) | Yes |
| `slug` | Activity slug (supports `{field}` substitution) | Yes |
| `template` | Content template: `generic`, `pipeline`, or `alert` (default: `generic`) | No |
| `priority` | Per-rule priority override (0-10, overrides global default) | No |
| `lifecycle` | `field` or `presence` | Yes |
| `state_field` | JSON field to read state from (required for `field` lifecycle) | field only |
| `state_map` | Map of field values to `ONGOING`, `ENDED`, or `IGNORE` (required for `field` lifecycle) | field only |
| `inactivity_timeout` | End activity after this duration without messages (required for `presence` lifecycle) | presence only |
| `content` | Content field mappings (see below) | Yes |

### Presence lifecycle

The activity stays `ONGOING` as long as messages keep arriving. When no message is received within `inactivity_timeout`, the activity ends via two-phase end.

```yaml
rules:
  - name: "Air Quality"
    topic: "sensors/+/air_quality"
    slug: "air-quality"
    template: "generic"
    lifecycle: "presence"
    inactivity_timeout: 5m
    content:
      state: "{aqi | format:AQI %0.f}"
      subtitle: "{_topic_segment:1}"
      icon:
        default: "aqi.medium"
        map:
          level:
            good: "aqi.low"
            moderate: "aqi.medium"
            unhealthy: "aqi.high"
      accent_color:
        default: "green"
        map:
          level:
            moderate: "orange"
            unhealthy: "red"
```

This example uses a wildcard topic (`sensors/+/air_quality`) and the virtual field `_topic_segment:1` to show the sensor name in the subtitle (e.g. for topic `sensors/bedroom/air_quality`, the subtitle resolves to `bedroom`).

## Template Strings

Content field values use `{field | transform}` syntax to extract and transform values from the MQTT JSON payload.

```yaml
state: "{running_state}"                         # direct field value
progress: "{completion_percentage | div:100}"     # divide by 100 → 0.0-1.0
remaining_time: "{remaining_minutes | mul:60}"    # minutes to seconds
state: "{aqi | format:AQI %0.f}"                 # format as "AQI 42"
subtitle: "{_topic_segment:1}"                    # virtual field from topic
```

Fields support dot-notation for nested JSON (e.g. `{sensor.temperature.value}`). Multiple transforms can be chained with ` | ` (space-pipe-space). Missing fields resolve to an empty string — use the `default` transform to provide a fallback.

## Pipe Transforms

| Transform | Description | Example | Result |
|---|---|---|---|
| `div:N` | Divide by N | `{percent \| div:100}` (value: 75) | `0.75` |
| `mul:N` | Multiply by N | `{minutes \| mul:60}` (value: 23) | `1380` |
| `format:F` | Printf-style format | `{temp \| format:%.1f°C}` (value: 23.456) | `23.5°C` |
| `scale:min:max` | Normalize to 0.0-1.0 range | `{value \| scale:0:1000}` (value: 500) | `0.5` |
| `default:V` | Fallback if field is nil or empty | `{name \| default:Unknown}` | `Unknown` |
| `upper` | Convert to uppercase | `{status \| upper}` (value: "ok") | `OK` |
| `lower` | Convert to lowercase | `{STATUS \| lower}` (value: "RUNNING") | `running` |

Transforms can be chained: `{value | div:100 | format:%.0f%%}` → `"75%"` (from value 7500).

## Conditional Mapping

The `icon` and `accent_color` fields support conditional values based on a JSON field. Use `default` for the base value and `map` to override based on field values:

```yaml
icon:
  default: "washer.fill"
  map:
    running_state:
      paused: "pause.circle.fill"
      done: "checkmark.circle.fill"

accent_color:
  default: "blue"
  map:
    running_state:
      paused: "orange"
      done: "green"
```

When a message arrives, the bridge checks each field in `map`. If the field's value matches a key, that value is used. Otherwise, `default` is returned.

## How It Works

1. **Connect** — establishes an MQTT connection to the broker (with optional TLS and credentials) and subscribes to all topics referenced by the configured rules
2. **Receive** — incoming messages are JSON-parsed and matched against rules by topic (including MQTT `+` and `#` wildcards)
3. **Virtual fields** — `_topic` (full topic string) and `_topic_segment:N` (Nth segment, 0-indexed) are injected into the message data
4. **Lifecycle check** — in `field` mode, the `state_field` value is looked up in `state_map` to determine `ONGOING`, `ENDED`, or `IGNORE`; in `presence` mode, any message resets the inactivity timer
5. **Activity creation** — on first `ONGOING`, creates a PushWard activity with the resolved slug, rule name, priority, and TTL values
6. **Content mapping** — template strings are resolved against the message data, transforms are applied, and conditional mappings are evaluated for icon and accent color
7. **Debounced update** — sends the resolved content to PushWard as an `ONGOING` update, throttled to the configured update interval (default 5s)
8. **Two-phase end** — on `ENDED` (or inactivity timeout), sends `ONGOING` with final content first (so it reaches the device via push-update token), then sends `ENDED` to dismiss the Live Activity

### Activity slug format

`mqtt-<resolved-slug>` (e.g. `mqtt-washer`, `mqtt-air-quality`). Slugs support `{field}` substitution for per-device activities (e.g. `slug: "z2m-{_topic_segment:1}"` → `mqtt-z2m-bedroom`). Uses the `generic` content template by default.

Recommended `activity_slugs` prefix for integration key: `mqtt-*`
