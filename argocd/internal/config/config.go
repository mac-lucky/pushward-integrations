package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	ArgoCD   ArgoCDConfig   `yaml:"argocd"`
	PushWard PushWardConfig `yaml:"pushward"`
}

type ServerConfig struct {
	Address string `yaml:"address"`
}

type ArgoCDConfig struct {
	WebhookSecret string `yaml:"webhook_secret"`
	URL           string `yaml:"url"`
}

type PushWardConfig struct {
	URL             string        `yaml:"url"`
	APIKey          string        `yaml:"api_key"`
	Priority        int           `yaml:"priority"`
	CleanupDelay    time.Duration `yaml:"cleanup_delay"`
	StaleTimeout    time.Duration `yaml:"stale_timeout"`
	SyncGracePeriod time.Duration `yaml:"sync_grace_period"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Address: ":8090",
		},
		PushWard: PushWardConfig{
			Priority:        3,
			CleanupDelay:    5 * time.Minute,
			StaleTimeout:    30 * time.Minute,
			SyncGracePeriod: 10 * time.Second,
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
	if v := os.Getenv("PUSHWARD_ARGOCD_WEBHOOK_SECRET"); v != "" {
		cfg.ArgoCD.WebhookSecret = v
	}
	if v := os.Getenv("PUSHWARD_ARGOCD_URL"); v != "" {
		cfg.ArgoCD.URL = v
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
	if v := os.Getenv("PUSHWARD_SYNC_GRACE_PERIOD"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("parsing PUSHWARD_SYNC_GRACE_PERIOD: %w", err)
		}
		cfg.PushWard.SyncGracePeriod = d
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
