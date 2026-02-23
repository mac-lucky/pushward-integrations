package config

import (
	"os"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-docker/shared/config"
)

type Config struct {
	Server   sharedconfig.ServerConfig   `yaml:"server"`
	Grafana  GrafanaConfig              `yaml:"grafana"`
	PushWard sharedconfig.PushWardConfig `yaml:"pushward"`
}

type GrafanaConfig struct {
	SeverityLabel   string `yaml:"severity_label"`
	DefaultSeverity string `yaml:"default_severity"`
	DefaultIcon     string `yaml:"default_icon"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: sharedconfig.ServerConfig{
			Address: ":8090",
		},
		Grafana: GrafanaConfig{
			SeverityLabel:   "severity",
			DefaultSeverity: "warning",
			DefaultIcon:     "exclamationmark.triangle.fill",
		},
		PushWard: sharedconfig.PushWardConfig{
			Priority:     5,
			CleanupDelay: 5 * time.Minute,
			StaleTimeout: 24 * time.Hour,
		},
	}

	if err := sharedconfig.LoadYAML(path, cfg); err != nil {
		return nil, err
	}

	// Integration-specific env overrides
	cfg.Server.ApplyEnvOverrides()
	if v := os.Getenv("PUSHWARD_GRAFANA_SEVERITY_LABEL"); v != "" {
		cfg.Grafana.SeverityLabel = v
	}
	if v := os.Getenv("PUSHWARD_GRAFANA_DEFAULT_SEVERITY"); v != "" {
		cfg.Grafana.DefaultSeverity = v
	}
	if v := os.Getenv("PUSHWARD_GRAFANA_DEFAULT_ICON"); v != "" {
		cfg.Grafana.DefaultIcon = v
	}

	// Shared PushWard env overrides
	if err := cfg.PushWard.ApplyEnvOverrides(); err != nil {
		return nil, err
	}

	// Shared validation
	if err := cfg.PushWard.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
