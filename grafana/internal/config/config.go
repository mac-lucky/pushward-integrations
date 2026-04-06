package config

import (
	"fmt"
	"os"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

// Config is the top-level configuration for pushward-grafana.
type Config struct {
	Server       sharedconfig.ServerConfig   `yaml:"server"`
	PushWard     sharedconfig.PushWardConfig `yaml:"pushward"`
	Metrics      MetricsConfig              `yaml:"metrics"`
	Grafana      GrafanaConfig              `yaml:"grafana"`
	Timeline     TimelineConfig             `yaml:"timeline"`
	WebhookToken string                     `yaml:"webhook_token"`
}

// MetricsConfig holds the Prometheus/VictoriaMetrics connection details.
type MetricsConfig struct {
	URL         string `yaml:"url"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	BearerToken string `yaml:"bearer_token"`
}

// GrafanaConfig holds optional Grafana API connection for auto-extracting queries.
type GrafanaConfig struct {
	URL      string `yaml:"url"`
	APIToken string `yaml:"api_token"` // Editor-role service account token
}

// TimelineConfig holds defaults for the timeline template.
type TimelineConfig struct {
	HistoryWindow   time.Duration `yaml:"history_window"`
	PollInterval    time.Duration `yaml:"poll_interval"`
	Smoothing       *bool         `yaml:"smoothing"`
	Scale           string        `yaml:"scale"`
	Decimals        *int          `yaml:"decimals"`
	SeverityLabel   string        `yaml:"severity_label"`
	DefaultSeverity string        `yaml:"default_severity"`
}

// Load reads the config file and applies environment variable overrides.
func Load(path string) (*Config, error) {
	smoothing := true
	decimals := 1
	cfg := &Config{
		Server: sharedconfig.ServerConfig{
			Address: ":8090",
		},
		PushWard: sharedconfig.PushWardConfig{
			Priority:     5,
			CleanupDelay: 15 * time.Minute,
			StaleTimeout: 24 * time.Hour,
		},
		Timeline: TimelineConfig{
			HistoryWindow:   30 * time.Minute,
			PollInterval:    30 * time.Second,
			Smoothing:       &smoothing,
			Scale:           "linear",
			Decimals:        &decimals,
			SeverityLabel:   "severity",
			DefaultSeverity: "warning",
		},
	}

	if err := sharedconfig.LoadYAML(path, cfg); err != nil {
		return nil, err
	}

	cfg.Server.ApplyEnvOverrides()
	if err := applyEnvOverrides(cfg); err != nil {
		return nil, err
	}

	if err := cfg.PushWard.ApplyEnvOverrides(); err != nil {
		return nil, err
	}
	if err := cfg.PushWard.Validate(); err != nil {
		return nil, err
	}

	if cfg.Metrics.URL == "" {
		return nil, fmt.Errorf("metrics.url is required (set PUSHWARD_METRICS_URL)")
	}

	return cfg, nil
}

// AutoExtractEnabled reports whether the Grafana API is configured for query auto-extraction.
func (c *Config) AutoExtractEnabled() bool {
	return c.Grafana.URL != "" && c.Grafana.APIToken != ""
}

func applyEnvOverrides(cfg *Config) error {
	if v := os.Getenv("PUSHWARD_METRICS_URL"); v != "" {
		cfg.Metrics.URL = v
	}
	if v := os.Getenv("PUSHWARD_METRICS_USERNAME"); v != "" {
		cfg.Metrics.Username = v
	}
	if v := os.Getenv("PUSHWARD_METRICS_PASSWORD"); v != "" {
		cfg.Metrics.Password = v
	}
	if v := os.Getenv("PUSHWARD_METRICS_BEARER_TOKEN"); v != "" {
		cfg.Metrics.BearerToken = v
	}
	if v := os.Getenv("PUSHWARD_GRAFANA_URL"); v != "" {
		cfg.Grafana.URL = v
	}
	if v := os.Getenv("PUSHWARD_GRAFANA_API_TOKEN"); v != "" {
		cfg.Grafana.APIToken = v
	}
	if v := os.Getenv("PUSHWARD_HISTORY_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid PUSHWARD_HISTORY_WINDOW %q: %w", v, err)
		}
		cfg.Timeline.HistoryWindow = d
	}
	if v := os.Getenv("PUSHWARD_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid PUSHWARD_POLL_INTERVAL %q: %w", v, err)
		}
		cfg.Timeline.PollInterval = d
	}
	if v := os.Getenv("PUSHWARD_WEBHOOK_TOKEN"); v != "" {
		cfg.WebhookToken = v
	}
	return nil
}
