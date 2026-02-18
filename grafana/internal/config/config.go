package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Grafana  GrafanaConfig  `yaml:"grafana"`
	PushWard PushWardConfig `yaml:"pushward"`
}

type ServerConfig struct {
	Address string `yaml:"address"`
}

type GrafanaConfig struct {
	SeverityLabel   string `yaml:"severity_label"`
	DefaultSeverity string `yaml:"default_severity"`
	DefaultIcon     string `yaml:"default_icon"`
}

type PushWardConfig struct {
	URL          string        `yaml:"url"`
	APIKey       string        `yaml:"api_key"`
	Priority     int           `yaml:"priority"`
	CleanupDelay time.Duration `yaml:"cleanup_delay"`
	StaleTimeout time.Duration `yaml:"stale_timeout"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Address: ":8090",
		},
		Grafana: GrafanaConfig{
			SeverityLabel:   "severity",
			DefaultSeverity: "warning",
			DefaultIcon:     "exclamationmark.triangle.fill",
		},
		PushWard: PushWardConfig{
			Priority:     5,
			CleanupDelay: 5 * time.Minute,
			StaleTimeout: 24 * time.Hour,
		},
	}

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config: %w", err)
		}
	}

	// Environment variable overrides
	if v := os.Getenv("PUSHWARD_SERVER_ADDRESS"); v != "" {
		cfg.Server.Address = v
	}
	if v := os.Getenv("PUSHWARD_GRAFANA_SEVERITY_LABEL"); v != "" {
		cfg.Grafana.SeverityLabel = v
	}
	if v := os.Getenv("PUSHWARD_GRAFANA_DEFAULT_SEVERITY"); v != "" {
		cfg.Grafana.DefaultSeverity = v
	}
	if v := os.Getenv("PUSHWARD_GRAFANA_DEFAULT_ICON"); v != "" {
		cfg.Grafana.DefaultIcon = v
	}
	if v := os.Getenv("PUSHWARD_URL"); v != "" {
		cfg.PushWard.URL = v
	}
	if v := os.Getenv("PUSHWARD_API_KEY"); v != "" {
		cfg.PushWard.APIKey = v
	}
	if v := os.Getenv("PUSHWARD_PRIORITY"); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err != nil {
			return nil, fmt.Errorf("parsing PUSHWARD_PRIORITY: %w", err)
		}
		cfg.PushWard.Priority = p
	}
	if v := os.Getenv("PUSHWARD_CLEANUP_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("parsing PUSHWARD_CLEANUP_DELAY: %w", err)
		}
		cfg.PushWard.CleanupDelay = d
	}
	if v := os.Getenv("PUSHWARD_STALE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("parsing PUSHWARD_STALE_TIMEOUT: %w", err)
		}
		cfg.PushWard.StaleTimeout = d
	}

	// Validation
	if cfg.PushWard.URL == "" {
		return nil, fmt.Errorf("pushward.url is required (set PUSHWARD_URL)")
	}
	if cfg.PushWard.APIKey == "" {
		return nil, fmt.Errorf("pushward.api_key is required (set PUSHWARD_API_KEY)")
	}
	if cfg.PushWard.Priority < 0 || cfg.PushWard.Priority > 10 {
		return nil, fmt.Errorf("pushward.priority must be 0-10 (got %d)", cfg.PushWard.Priority)
	}

	return cfg, nil
}
