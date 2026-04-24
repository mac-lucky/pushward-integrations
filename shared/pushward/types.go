package pushward

// Activity state constants.
const (
	StateOngoing = "ONGOING"
	StateEnded   = "ENDED"
)

// Template name constants.
const (
	TemplateGeneric   = "generic"
	TemplateAlert     = "alert"
	TemplateSteps     = "steps"
	TemplateCountdown = "countdown"
	TemplateGauge     = "gauge"
	TemplateTimeline  = "timeline"
)

// Notification interruption level constants.
const (
	LevelActive  = "active"
	LevelPassive = "passive"
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
	T int64   `json:"t"` // Unix timestamp (seconds)
	V float64 `json:"v"` // Value
}

// Threshold defines a horizontal reference line on a timeline sparkline.
type Threshold struct {
	Value float64 `json:"value"`
	Color string  `json:"color,omitempty"`
	Label string  `json:"label,omitempty"`
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

	// Alert template
	Severity string `json:"severity,omitempty"`
	FiredAt  *int64 `json:"fired_at,omitempty"`

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
	// partial updates until cleared via Client.ClearActivityAlarm or a
	// transition to ENDED. iOS 26+ only.
	Alarm *bool `json:"alarm,omitempty"`

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
}

// CreateActivityRequest is the body for POST /activities.
type CreateActivityRequest struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Priority int    `json:"priority"`
	EndedTTL int    `json:"ended_ttl,omitempty"`
	StaleTTL int    `json:"stale_ttl,omitempty"`
}

// UpdateRequest is the body for the full-content PATCH /activity/{slug} used
// to seed a session or close it out with a final ENDED frame. For partial
// updates mid-session, prefer Client.PatchActivity with a ContentPatch.
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

	// Alert template
	Severity *string `json:"severity,omitempty"`
	FiredAt  *int64  `json:"fired_at,omitempty"`

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
}

// PatchRequest is the typed body for PATCH /activity/{slug}. State is a plain
// string with omitempty so that tick updates can leave it unset and rely on
// the server preserving the stored state.
type PatchRequest struct {
	State    string        `json:"state,omitempty"`
	Content  *ContentPatch `json:"content,omitempty"`
	Sound    ActivitySound `json:"sound,omitempty"`
	Priority *int          `json:"priority,omitempty"`
}

// SendNotificationRequest is the body for POST /notifications.
type SendNotificationRequest struct {
	Title             string            `json:"title"`
	Subtitle          string            `json:"subtitle,omitempty"`
	Body              string            `json:"body"`
	ThreadID          string            `json:"thread_id,omitempty"`
	CollapseID        string            `json:"collapse_id,omitempty"`
	Level             string            `json:"level,omitempty"`
	Category          string            `json:"category,omitempty"`
	Source            string            `json:"source,omitempty"`
	SourceDisplayName string            `json:"source_display_name,omitempty"`
	URL               string            `json:"url,omitempty"`
	ImageURL          string            `json:"image_url,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	Push              bool              `json:"push"`
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
	"unraid":          "Unraid",
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
