package config

import (
	"fmt"
	"os"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

type Config struct {
	Server   sharedconfig.ServerConfig   `yaml:"server"`
	ArgoCD   ArgoCDConfig               `yaml:"argocd"`
	PushWard sharedconfig.PushWardConfig `yaml:"pushward"`
}

type ArgoCDConfig struct {
	WebhookSecret   string        `yaml:"webhook_secret"`
	URL             string        `yaml:"url"`
	SyncGracePeriod time.Duration `yaml:"sync_grace_period"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: sharedconfig.ServerConfig{
			Address: ":8090",
		},
		ArgoCD: ArgoCDConfig{
			SyncGracePeriod: 10 * time.Second,
		},
		PushWard: sharedconfig.PushWardConfig{
			Priority:       3,
			CleanupDelay:   5 * time.Minute,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       5 * time.Second,
			EndDisplayTime: 4 * time.Second,
		},
	}

	if err := sharedconfig.LoadYAML(path, cfg); err != nil {
		return nil, err
	}

	// Integration-specific env overrides
	cfg.Server.ApplyEnvOverrides()
	if v := os.Getenv("PUSHWARD_ARGOCD_WEBHOOK_SECRET"); v != "" {
		cfg.ArgoCD.WebhookSecret = v
	}
	if v := os.Getenv("PUSHWARD_ARGOCD_URL"); v != "" {
		cfg.ArgoCD.URL = v
	}
	if v := os.Getenv("PUSHWARD_SYNC_GRACE_PERIOD"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("parsing PUSHWARD_SYNC_GRACE_PERIOD: %w", err)
		}
		cfg.ArgoCD.SyncGracePeriod = d
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
