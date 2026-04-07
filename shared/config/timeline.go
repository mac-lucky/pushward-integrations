package config

// TimelineConfig holds visual display settings for the timeline template,
// shared across integrations that render timeline-style push notifications.
type TimelineConfig struct {
	Smoothing *bool  `yaml:"smoothing"`
	Scale     string `yaml:"scale"`
	Decimals  *int   `yaml:"decimals"`
}
