package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// PushWardConfig holds the common PushWard API settings shared by all integrations.
type PushWardConfig struct {
	URL            string        `yaml:"url"`
	APIKey         string        `yaml:"api_key"`
	Priority       int           `yaml:"priority"`
	CleanupDelay   time.Duration `yaml:"cleanup_delay"`
	StaleTimeout   time.Duration `yaml:"stale_timeout"`
	EndDelay       time.Duration `yaml:"end_delay"`
	EndDisplayTime time.Duration `yaml:"end_display_time"`
}

// ServerConfig holds the HTTP server settings for webhook-based integrations.
type ServerConfig struct {
	Address string `yaml:"address"`
}

// LoadYAML reads a YAML config file into target. Missing files are tolerated (ENOENT).
func LoadYAML(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading config: %w", err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, target); err != nil {
			return fmt.Errorf("parsing config: %w", err)
		}
	}
	return nil
}

// ApplyEnvOverrides applies PUSHWARD_* environment variable overrides.
func (c *PushWardConfig) ApplyEnvOverrides() error {
	if v := os.Getenv("PUSHWARD_URL"); v != "" {
		c.URL = v
	}
	if v := os.Getenv("PUSHWARD_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("PUSHWARD_PRIORITY"); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err != nil {
			return fmt.Errorf("parsing PUSHWARD_PRIORITY: %w", err)
		}
		c.Priority = p
	}
	if v := os.Getenv("PUSHWARD_CLEANUP_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parsing PUSHWARD_CLEANUP_DELAY: %w", err)
		}
		c.CleanupDelay = d
	}
	if v := os.Getenv("PUSHWARD_STALE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parsing PUSHWARD_STALE_TIMEOUT: %w", err)
		}
		c.StaleTimeout = d
	}
	if v := os.Getenv("PUSHWARD_END_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parsing PUSHWARD_END_DELAY: %w", err)
		}
		c.EndDelay = d
	}
	if v := os.Getenv("PUSHWARD_END_DISPLAY_TIME"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parsing PUSHWARD_END_DISPLAY_TIME: %w", err)
		}
		c.EndDisplayTime = d
	}
	return nil
}

// Validate checks that required fields are set and priority is in range.
func (c *PushWardConfig) Validate() error {
	if c.URL == "" {
		return fmt.Errorf("pushward.url is required (set PUSHWARD_URL)")
	}
	if c.APIKey == "" {
		return fmt.Errorf("pushward.api_key is required (set PUSHWARD_API_KEY)")
	}
	if c.Priority < 0 || c.Priority > 10 {
		return fmt.Errorf("pushward.priority must be 0-10 (got %d)", c.Priority)
	}
	return nil
}

// ApplyEnvOverrides applies the PUSHWARD_SERVER_ADDRESS environment variable.
func (c *ServerConfig) ApplyEnvOverrides() {
	if v := os.Getenv("PUSHWARD_SERVER_ADDRESS"); v != "" {
		c.Address = v
	}
}
