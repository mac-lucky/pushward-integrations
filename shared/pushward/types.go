package pushward

// Content is the superset of all content fields used across integrations.
// Unused fields use omitempty and won't appear in JSON.
type Content struct {
	Template      string  `json:"template"`
	Progress      float64 `json:"progress"`
	State         string  `json:"state,omitempty"`
	Icon          string  `json:"icon,omitempty"`
	Subtitle      string  `json:"subtitle,omitempty"`
	AccentColor   string  `json:"accent_color,omitempty"`
	CurrentStep   *int    `json:"current_step,omitempty"`
	TotalSteps    *int    `json:"total_steps,omitempty"`
	StepRows      []int   `json:"step_rows,omitempty"`
	URL           string  `json:"url,omitempty"`
	SecondaryURL  string  `json:"secondary_url,omitempty"`
	Severity      string  `json:"severity,omitempty"`
	FiredAt       *int64  `json:"fired_at,omitempty"`
	RemainingTime *int    `json:"remaining_time,omitempty"`
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
