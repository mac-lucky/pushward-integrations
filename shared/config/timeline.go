package config

import "github.com/mac-lucky/pushward-integrations/shared/pushward"

// TimelineConfig holds visual display settings for the timeline template,
// shared across integrations that render timeline-style push notifications.
type TimelineConfig struct {
	Smoothing *bool  `yaml:"smoothing"`
	Scale     string `yaml:"scale"`
	Decimals  *int   `yaml:"decimals"`
}

// Apply sets the timeline display fields on a Content struct.
func (tc TimelineConfig) Apply(c *pushward.Content) {
	c.Smoothing = tc.Smoothing
	c.Scale = tc.Scale
	c.Decimals = tc.Decimals
}
