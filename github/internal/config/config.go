package config

import (
	"fmt"
	"os"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

type Config struct {
	GitHub   GitHubConfig                `yaml:"github"`
	PushWard sharedconfig.PushWardConfig `yaml:"pushward"`
	Polling  sharedconfig.PollingConfig  `yaml:"polling"`
	Render   sharedconfig.RenderConfig   `yaml:"render"`
}

type GitHubConfig struct {
	Token string   `yaml:"token"`
	Owner string   `yaml:"owner"`
	Repos []string `yaml:"repos"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		PushWard: sharedconfig.DefaultPushWardConfig(),
		Polling:  sharedconfig.DefaultPollingConfig(),
		Render:  sharedconfig.DefaultRenderConfig(),
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
	if err := cfg.Polling.ApplyEnvOverrides(); err != nil {
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
	// Derive before checking, and only once every override layer above has run: the
	// active tier comes from whatever idle_interval survived them.
	cfg.Polling.ApplyActiveDefault()
	if err := cfg.Polling.Validate(); err != nil {
		return nil, err
	}

	// Shared validation
	if err := cfg.PushWard.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
