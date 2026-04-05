package pushward

// Activity state constants.
const (
	StateOngoing = "ONGOING"
	StateEnded   = "ENDED"
)

// IntPtr returns a pointer to the given int value.
func IntPtr(v int) *int { return &v }

// Int64Ptr returns a pointer to the given int64 value.
func Int64Ptr(v int64) *int64 { return &v }

// Float64Ptr returns a pointer to the given float64 value.
func Float64Ptr(v float64) *float64 { return &v }

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

	// Gauge template
	Value    *float64 `json:"value,omitempty"`
	MinValue *float64 `json:"min_value,omitempty"`
	MaxValue *float64 `json:"max_value,omitempty"`
	Unit     string   `json:"unit,omitempty"`

	// Timeline template
	Scale      string             `json:"scale,omitempty"`
	Decimals   *int               `json:"decimals,omitempty"`
	Smoothing  *bool              `json:"smoothing,omitempty"`
	Thresholds []Threshold        `json:"thresholds,omitempty"`
	Values     map[string]float64 `json:"values,omitempty"`
	Units      map[string]string  `json:"units,omitempty"`
}

// CreateActivityRequest is the body for POST /activities.
type CreateActivityRequest struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Priority int    `json:"priority"`
	EndedTTL int    `json:"ended_ttl,omitempty"`
	StaleTTL int    `json:"stale_ttl,omitempty"`
}

// UpdateRequest is the body for PATCH /activity/{slug}.
type UpdateRequest struct {
	State   string  `json:"state"`
	Content Content `json:"content"`
}
