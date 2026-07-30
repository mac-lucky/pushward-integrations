package config

import (
	"fmt"
	"os"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

type Config struct {
	GitHub   GitHubConfig                `yaml:"github"`
	PushWard sharedconfig.PushWardConfig `yaml:"pushward"`
	Polling  PollingConfig               `yaml:"polling"`
	Render   sharedconfig.RenderConfig   `yaml:"render"`
}

type GitHubConfig struct {
	Token string   `yaml:"token"`
	Owner string   `yaml:"owner"`
	Repos []string `yaml:"repos"`
}

type PollingConfig struct {
	IdleInterval time.Duration `yaml:"idle_interval"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		PushWard: sharedconfig.PushWardConfig{
			Priority:       1,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       5 * time.Second,
			EndDisplayTime: 4 * time.Second,
		},
		Polling: PollingConfig{
			IdleInterval: 60 * time.Second,
		},
		Render: sharedconfig.DefaultRenderConfig(),
	}

	if err := sharedconfig.LoadYAML(path, cfg); err != nil {
		return nil, err
	}

	// Integration-specific env overrides
	if v := os.Getenv("PUSHWARD_GITHUB_TOKEN"); v != "" {
		cfg.GitHub.Token = v
	}
	if v := os.Getenv("PUSHWARD_GITHUB_OWNER"); v != "" {
		cfg.GitHub.Owner = v
	}
	if v := os.Getenv("PUSHWARD_GITHUB_REPOS"); v != "" {
		cfg.GitHub.Repos = sharedconfig.SplitList(v)
	}
	if err := sharedconfig.EnvDuration("PUSHWARD_POLL_IDLE", &cfg.Polling.IdleInterval); err != nil {
		return nil, err
	}
	if err := cfg.Render.ApplyEnvOverrides("PUSHWARD_GITHUB"); err != nil {
		return nil, err
	}

	// Shared PushWard env overrides
	if err := cfg.PushWard.ApplyEnvOverrides(); err != nil {
		return nil, err
	}

	// Integration-specific validation
	if cfg.GitHub.Token == "" {
		return nil, fmt.Errorf("github.token is required (set PUSHWARD_GITHUB_TOKEN)")
	}
	if len(cfg.GitHub.Repos) == 0 && cfg.GitHub.Owner == "" {
		return nil, fmt.Errorf("github.repos or github.owner is required (set PUSHWARD_GITHUB_REPOS or PUSHWARD_GITHUB_OWNER)")
	}
	if cfg.Polling.IdleInterval <= 0 {
		return nil, fmt.Errorf("polling.idle_interval must be positive, got %s", cfg.Polling.IdleInterval)
	}

	// Shared validation
	if err := cfg.PushWard.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
