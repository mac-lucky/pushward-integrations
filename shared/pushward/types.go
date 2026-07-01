package pushward

// Activity state constants.
const (
	StateOngoing = "ongoing"
	StateEnded   = "ended"
)

// Template name constants.
const (
	TemplateGeneric   = "generic"
	TemplateAlert     = "alert"
	TemplateSteps     = "steps"
	TemplateCountdown = "countdown"
	TemplateGauge     = "gauge"
	TemplateTimeline  = "timeline"
	TemplateBoard     = "board"
	TemplateLog       = "log"
)

// Trend direction constants annotate a board tile (and value/gauge widget)
// with a directional arrow. Mirrors the server's TrendDirection enum.
const (
	TrendUp   = "up"
	TrendDown = "down"
	TrendFlat = "flat"
)

// Log level constants tag an individual log-template line. Mirrors the
// server's LogLevel enum.
const (
	LogInfo  = "info"
	LogWarn  = "warn"
	LogError = "error"
)

// Notification interruption level constants.
const (
	LevelActive        = "active"
	LevelPassive       = "passive"
	LevelTimeSensitive = "time-sensitive"
	LevelCritical      = "critical"
)

// ActivitySound is a Live Activity alert-sound identifier. The typed alias
// stops the value being confused with other string arguments at call sites.
// Any string is accepted by the SDK — the server is the source of truth for
// the allowlist (it returns 400 on unrecognised values), so clients don't
// mirror it and avoid drift as new sounds are added server-side.
type ActivitySound string

// BoolPtr returns a pointer to the given bool value.
func BoolPtr(v bool) *bool { return &v }

// IntPtr returns a pointer to the given int value.
func IntPtr(v int) *int { return &v }

// Int64Ptr returns a pointer to the given int64 value.
func Int64Ptr(v int64) *int64 { return &v }

// Float64Ptr returns a pointer to the given float64 value.
func Float64Ptr(v float64) *float64 { return &v }

// StringPtr returns a pointer to the given string value.
func StringPtr(v string) *string { return &v }

// HistoryPoint is a single timestamped value in a timeline series.
type HistoryPoint struct {
	Timestamp int64   `json:"timestamp"` // Unix timestamp (seconds)
	Value     float64 `json:"value"`     // Value
}

// Threshold defines a horizontal reference line on a timeline sparkline.
type Threshold struct {
	Value float64 `json:"value"`
	Color string  `json:"color,omitempty"`
	Label string  `json:"label,omitempty"`
}

// TapAction is a routed tap target on a Live Activity (or widget). It mirrors
// the server's model.TapAction and NotificationAction routing fields, so iOS
// dispatches a tap the same way across notifications and Live Activities. The
// behavior is inferred from the URL scheme + Foreground flag:
//   - custom scheme (e.g. youtube://, homeassistant://) → opens that app
//   - http(s) + Foreground=true → opens the URL in Safari / in-app browser
//   - http(s) + Foreground=false → silent webhook (Method/Headers/Body honored)
//
// Title and Icon are only meaningful when the action is rendered as a button
// (url_action / secondary_url_action); the widget-wide tap_action ignores them.
type TapAction struct {
	URL        string            `json:"url"`
	Foreground bool              `json:"foreground,omitempty"`
	Method     string            `json:"method,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	Title      string            `json:"title,omitempty"`
	Icon       string            `json:"icon,omitempty"`
}

// BoardTile is a single cell in a board template (1-4 per activity). Value is a
// string so callers can render non-numeric states ("Open", "On") alongside
// numbers. Trend is one of TrendUp/TrendDown/TrendFlat. URLAction reuses the
// shared TapAction routing type so a tile tap behaves like any other target.
type BoardTile struct {
	Label     string     `json:"label"`
	Value     string     `json:"value"`
	Unit      string     `json:"unit,omitempty"`
	Icon      string     `json:"icon,omitempty"`
	Color     string     `json:"color,omitempty"`
	Trend     string     `json:"trend,omitempty"`
	URLAction *TapAction `json:"url_action,omitempty"`
}

// LogLine is a single entry in a log template (1-20 per activity, newest-first).
// At is an optional unix timestamp (seconds); Level is an optional severity tag
// (LogInfo/LogWarn/LogError). The server accumulates a rolling backlog of lines
// server-side; that backlog is read-only and never sent by clients.
type LogLine struct {
	Text  string `json:"text"`
	At    *int64 `json:"at,omitempty"`
	Level string `json:"level,omitempty"`
}

// Content is the superset of all content fields used across integrations.
// Unused fields use omitempty and won't appear in JSON.
type Content struct {
	// Core fields (all templates)
	Template        string  `json:"template"`
	Progress        float64 `json:"progress"`
	State           string  `json:"state,omitempty"`
	Icon            string  `json:"icon,omitempty"`
	Subtitle        string  `json:"subtitle,omitempty"`
	AccentColor     string  `json:"accent_color,omitempty"`
	BackgroundColor string  `json:"background_color,omitempty"`
	TextColor       string  `json:"text_color,omitempty"`
	RemainingTime   *int    `json:"remaining_time,omitempty"`
	URL             string  `json:"url,omitempty"`
	SecondaryURL    string  `json:"secondary_url,omitempty"`

	// Tap-action routing (any template). tap_action overrides the widget-wide
	// tap target; url_action / secondary_url_action render as routed buttons
	// alongside (and taking precedence over) the legacy URL / SecondaryURL
	// string fields.
	TapAction          *TapAction `json:"tap_action,omitempty"`
	URLAction          *TapAction `json:"url_action,omitempty"`
	SecondaryURLAction *TapAction `json:"secondary_url_action,omitempty"`

	// Alert template
	Severity      string `json:"severity,omitempty"`
	FiredAt       *int64 `json:"fired_at,omitempty"`
	SeverityLabel string `json:"severity_label,omitempty"`

	// Steps template
	CurrentStep *int     `json:"current_step,omitempty"`
	TotalSteps  *int     `json:"total_steps,omitempty"`
	StepRows    []int    `json:"step_rows,omitempty"`
	StepLabels  []string `json:"step_labels,omitempty"`

	// Countdown template
	Duration          *string `json:"duration,omitempty"`
	EndDate           *int64  `json:"end_date,omitempty"`
	StartDate         *int64  `json:"start_date,omitempty"`
	WarningThreshold  *int    `json:"warning_threshold,omitempty"`
	CompletionMessage string  `json:"completion_message,omitempty"`
	// Alarm opts in to iOS 26 AlarmKit scheduling at end_date. Persists across
	// partial merge-patch updates until cleared by a transition to ENDED or by
	// patching content.alarm to explicit null. iOS 26+ only.
	Alarm *bool `json:"alarm,omitempty"`
	// SnoozeSeconds sets how far POST /activities/{slug}/snooze extends end_date
	// (and the iOS AlarmKit snooze window). 60–3600; server defaults to 300 when
	// omitted. Only meaningful with Alarm set.
	SnoozeSeconds *int `json:"snooze_seconds,omitempty"`

	// Gauge template: Value is float64
	// Timeline template: Value is map[string]float64
	Value    any      `json:"value,omitempty"`
	MinValue *float64 `json:"min_value,omitempty"`
	MaxValue *float64 `json:"max_value,omitempty"`
	Unit     string   `json:"unit,omitempty"`

	// Timeline template
	Scale      string                    `json:"scale,omitempty"`
	Decimals   *int                      `json:"decimals,omitempty"`
	Smoothing  *bool                     `json:"smoothing,omitempty"`
	Thresholds []Threshold               `json:"thresholds,omitempty"`
	Units      map[string]string         `json:"units,omitempty"`
	History    map[string][]HistoryPoint `json:"history,omitempty"`

	// Board template: 1-4 tiles, replaced wholesale on each update.
	Tiles []BoardTile `json:"tiles,omitempty"`

	// Log template: 1-20 lines, newest-first, replaced wholesale on each
	// update. The server-accumulated log_backlog is read-only and omitted here
	// (this client never reads it).
	Lines []LogLine `json:"lines,omitempty"`
}

// CreateActivityRequest is the body for POST /activities.
type CreateActivityRequest struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Priority int    `json:"priority"`
	EndedTTL int    `json:"ended_ttl,omitempty"`
	StaleTTL int    `json:"stale_ttl,omitempty"`
}

// UpdateRequest is the body for the full-content PATCH /activities/{slug}
// used to seed a session or close it out with a final ENDED frame. For
// partial updates mid-session, prefer Client.PatchActivity with a
// ContentPatch.
type UpdateRequest struct {
	State    string        `json:"state,omitempty"`
	Content  Content       `json:"content"`
	Sound    ActivitySound `json:"sound,omitempty"`
	Priority *int          `json:"priority,omitempty"`
}

// ContentPatch is the typed body for partial content updates. Unset pointer
// fields are omitted and preserved server-side under RFC 7396 merge-patch
// semantics. Use with Client.PatchActivity.
//
// Every pointer field MUST carry `json:",omitempty"` — without it a nil
// pointer marshals as JSON null, which per RFC 7396 §2 instructs the server
// to delete the field. Adding a new pointer field without omitempty is a
// silent correctness bug.
type ContentPatch struct {
	Template        *string  `json:"template,omitempty"`
	Progress        *float64 `json:"progress,omitempty"`
	State           *string  `json:"state,omitempty"`
	Icon            *string  `json:"icon,omitempty"`
	Subtitle        *string  `json:"subtitle,omitempty"`
	AccentColor     *string  `json:"accent_color,omitempty"`
	BackgroundColor *string  `json:"background_color,omitempty"`
	TextColor       *string  `json:"text_color,omitempty"`
	RemainingTime   *int     `json:"remaining_time,omitempty"`
	URL             *string  `json:"url,omitempty"`
	SecondaryURL    *string  `json:"secondary_url,omitempty"`

	// Tap-action routing (any template). Each slot is a *TapAction: nil is
	// omitted (preserve server-side). A present value is deep-merged into the
	// stored action per RFC 7396 (the set fields overwrite, omitted fields are
	// preserved), so send the whole action to fully replace it.
	TapAction          *TapAction `json:"tap_action,omitempty"`
	URLAction          *TapAction `json:"url_action,omitempty"`
	SecondaryURLAction *TapAction `json:"secondary_url_action,omitempty"`

	// Alert template
	Severity      *string `json:"severity,omitempty"`
	FiredAt       *int64  `json:"fired_at,omitempty"`
	SeverityLabel *string `json:"severity_label,omitempty"`

	// Steps template
	CurrentStep *int     `json:"current_step,omitempty"`
	TotalSteps  *int     `json:"total_steps,omitempty"`
	StepRows    []int    `json:"step_rows,omitempty"`
	StepLabels  []string `json:"step_labels,omitempty"`

	// Countdown template
	Duration          *string `json:"duration,omitempty"`
	EndDate           *int64  `json:"end_date,omitempty"`
	StartDate         *int64  `json:"start_date,omitempty"`
	WarningThreshold  *int    `json:"warning_threshold,omitempty"`
	CompletionMessage *string `json:"completion_message,omitempty"`
	Alarm             *bool   `json:"alarm,omitempty"`
	SnoozeSeconds     *int    `json:"snooze_seconds,omitempty"`

	// Gauge template: Value is float64
	// Timeline template: Value is map[string]float64
	Value    any      `json:"value,omitempty"`
	MinValue *float64 `json:"min_value,omitempty"`
	MaxValue *float64 `json:"max_value,omitempty"`
	Unit     *string  `json:"unit,omitempty"`

	// Timeline template
	Scale      *string                   `json:"scale,omitempty"`
	Decimals   *int                      `json:"decimals,omitempty"`
	Smoothing  *bool                     `json:"smoothing,omitempty"`
	Thresholds []Threshold               `json:"thresholds,omitempty"`
	Units      map[string]string         `json:"units,omitempty"`
	History    map[string][]HistoryPoint `json:"history,omitempty"`

	// Board template: 1-4 tiles. Sending the slice replaces all tiles
	// (RFC 7396 array semantics); omitting it preserves the stored tiles.
	Tiles []BoardTile `json:"tiles,omitempty"`

	// Log template: 1-20 lines, newest-first. Sending the slice replaces the
	// live line snapshot; omitting it preserves the stored lines.
	Lines []LogLine `json:"lines,omitempty"`
}

// PatchRequest is the typed body for PATCH /activities/{slug}. State is a
// plain string with omitempty so that tick updates can leave it unset and
// rely on the server preserving the stored state.
type PatchRequest struct {
	State    string        `json:"state,omitempty"`
	Content  *ContentPatch `json:"content,omitempty"`
	Sound    ActivitySound `json:"sound,omitempty"`
	Priority *int          `json:"priority,omitempty"`
}

// WidgetTemplate names a renderer on the iOS widget extension.
type WidgetTemplate string

const (
	WidgetTemplateValue    WidgetTemplate = "value"
	WidgetTemplateProgress WidgetTemplate = "progress"
	WidgetTemplateStatus   WidgetTemplate = "status"
	WidgetTemplateGauge    WidgetTemplate = "gauge"
	WidgetTemplateStatList WidgetTemplate = "stat_list"
)

// StatRow is a single row of a stat_list widget. Value is pre-formatted by
// the integration (server does not localize / round); Unit is optional and
// rendered after the value.
type StatRow struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Unit  string `json:"unit,omitempty"`
}

// WidgetContent mirrors the server's widget content model. All fields are
// optional and respect RFC 7396 merge-patch semantics when sent via
// Client.UpdateWidget: pointer fields omitted with nil + omitempty are
// preserved, present pointer fields overwrite, and explicit JSON null on a
// pointer (achieved only by removing omitempty) clears the field server-side.
type WidgetContent struct {
	Template        WidgetTemplate `json:"template,omitempty"`
	Icon            string         `json:"icon,omitempty"`
	Value           *float64       `json:"value,omitempty"`
	MinValue        *float64       `json:"min_value,omitempty"`
	MaxValue        *float64       `json:"max_value,omitempty"`
	Unit            string         `json:"unit,omitempty"`
	Label           string         `json:"label,omitempty"`
	Subtitle        string         `json:"subtitle,omitempty"`
	Severity        string         `json:"severity,omitempty"`
	AccentColor     string         `json:"accent_color,omitempty"`
	BackgroundColor string         `json:"background_color,omitempty"`
	TextColor       string         `json:"text_color,omitempty"`
	// Trend annotates value/gauge widgets with a directional arrow. One of
	// "up" / "down" / "flat". Ignored for other templates.
	Trend string `json:"trend,omitempty"`
	// StatRows powers the stat_list template — a 1-6 row label/value list.
	// Required when template == stat_list, ignored otherwise.
	StatRows []StatRow `json:"stat_rows,omitempty"`
	// Tap-action routing on a widget. tap_action overrides the whole-widget tap
	// target; url_action / secondary_url_action render as routed buttons. Mirrors
	// the same slots on activity Content.
	TapAction          *TapAction `json:"tap_action,omitempty"`
	URLAction          *TapAction `json:"url_action,omitempty"`
	SecondaryURLAction *TapAction `json:"secondary_url_action,omitempty"`
}

// CreateWidgetRequest is the body for POST /widgets. The server upserts on
// (user, slug); a duplicate slug is not an error.
type CreateWidgetRequest struct {
	Slug         string        `json:"slug"`
	Name         string        `json:"name"`
	Content      WidgetContent `json:"content"`
	PushThrottle *int          `json:"push_throttle,omitempty"`
}

// UpdateWidgetRequest is the body for PATCH /widgets/{slug}. The server
// requires Content-Type "application/merge-patch+json" and applies RFC 7396
// merge semantics: present top-level fields overwrite; absent fields are
// preserved.
//
// Content is a pointer so that callers who only want to patch Name or
// PushThrottle leave it nil and the field is omitted from the wire payload
// entirely. Sending `"content":{}` would otherwise round-trip an empty
// struct through the server's struct-typed handler and risk clearing
// existing content fields.
type UpdateWidgetRequest struct {
	Name         string         `json:"name,omitempty"`
	Content      *WidgetContent `json:"content,omitempty"`
	PushThrottle *int           `json:"push_throttle,omitempty"`
}

// MediaAttachment is a rich media attachment (image, video, or audio)
// attached to a notification. The iOS client downloads the URL and
// attaches it via UNNotificationAttachment subject to Apple's per-type
// size caps (image 10 MB, audio 5 MB, video 50 MB). HTTPS only.
type MediaAttachment struct {
	URL  string `json:"url"`
	Type string `json:"type"` // "image" | "video" | "audio"
}

// MediaImage returns a MediaAttachment for an image URL, or nil if the
// URL is empty. Convenience for integrations that forward poster art,
// monitoring screenshots, or other image media without typing the full
// literal at every call site.
func MediaImage(url string) *MediaAttachment {
	if url == "" {
		return nil
	}
	return &MediaAttachment{URL: url, Type: "image"}
}

// NotificationAction is one server-driven action button shown on a push
// notification. Tapping the button surfaces the action's `id` to the iOS
// app, which routes by ID and opens the action's `url` if set.
type NotificationAction struct {
	ID                     string `json:"id"`
	Title                  string `json:"title"`
	URL                    string `json:"url,omitempty"`
	Foreground             bool   `json:"foreground,omitempty"`
	Destructive            bool   `json:"destructive,omitempty"`
	AuthenticationRequired bool   `json:"authentication_required,omitempty"`
	Icon                   string `json:"icon,omitempty"` // SF Symbol name
}

// SendNotificationRequest is the body for POST /notifications.
type SendNotificationRequest struct {
	Title             string               `json:"title"`
	Subtitle          string               `json:"subtitle,omitempty"`
	Body              string               `json:"body"`
	ThreadID          string               `json:"thread_id,omitempty"`
	CollapseID        string               `json:"collapse_id,omitempty"`
	Level             string               `json:"level,omitempty"`
	Volume            *float64             `json:"volume,omitempty"` // Sound volume for critical alerts (0.0-1.0)
	Source            string               `json:"source,omitempty"`
	SourceDisplayName string               `json:"source_display_name,omitempty"`
	URL               string               `json:"url,omitempty"`
	Media             *MediaAttachment     `json:"media,omitempty"`
	IconURL           string               `json:"icon_url,omitempty"`
	Metadata          map[string]string    `json:"metadata,omitempty"`
	Actions           []NotificationAction `json:"actions,omitempty"`
	Push              bool                 `json:"push"`
}

// sourceDisplayNames maps source identifiers to their human-readable display names.
var sourceDisplayNames = map[string]string{
	"grafana":         "Grafana",
	"argocd":          "ArgoCD",
	"radarr":          "Radarr",
	"sonarr":          "Sonarr",
	"prowlarr":        "Prowlarr",
	"jellyfin":        "Jellyfin",
	"paperless":       "Paperless-ngx",
	"changedetection": "Changedetection.io",
	"unmanic":         "Unmanic",
	"bazarr":          "Bazarr",
	"proxmox":         "Proxmox",
	"overseerr":       "Overseerr",
	"uptimekuma":      "Uptime Kuma",
	"gatus":           "Gatus",
	"backrest":        "Backrest",
	"sabnzbd":         "SABnzbd",
	"github":          "GitHub",
	"bambulab":        "BambuLab",
	"octoprint":       "OctoPrint",
	"mqtt":            "MQTT",
}

// DisplayNameFor returns the display name for a source, falling back to the identifier itself.
func DisplayNameFor(source string) string {
	if name, ok := sourceDisplayNames[source]; ok {
		return name
	}
	return source
}

// FillSourceDisplayName sets SourceDisplayName from Source when not already set.
func (r *SendNotificationRequest) FillSourceDisplayName() {
	if r.SourceDisplayName == "" && r.Source != "" {
		r.SourceDisplayName = DisplayNameFor(r.Source)
	}
}
