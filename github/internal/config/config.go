package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	GitHub   GitHubConfig   `yaml:"github"`
	PushWard PushWardConfig `yaml:"pushward"`
	Polling  PollingConfig  `yaml:"polling"`
}

type GitHubConfig struct {
	Token string   `yaml:"token"`
	Repos []string `yaml:"repos"`
}

type PushWardConfig struct {
	URL          string `yaml:"url"`
	APIKey       string `yaml:"api_key"`
	ActivitySlug string `yaml:"activity_slug"`
}

type PollingConfig struct {
	IdleInterval   time.Duration `yaml:"idle_interval"`
	ActiveInterval time.Duration `yaml:"active_interval"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Polling: PollingConfig{
			IdleInterval:   60 * time.Second,
			ActiveInterval: 5 * time.Second,
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
	if v := os.Getenv("PUSHWARD_GITHUB_TOKEN"); v != "" {
		cfg.GitHub.Token = v
	}
	if v := os.Getenv("PUSHWARD_GITHUB_REPOS"); v != "" {
		cfg.GitHub.Repos = strings.Split(v, ",")
	}
	if v := os.Getenv("PUSHWARD_URL"); v != "" {
		cfg.PushWard.URL = v
	}
	if v := os.Getenv("PUSHWARD_API_KEY"); v != "" {
		cfg.PushWard.APIKey = v
	}
	if v := os.Getenv("PUSHWARD_ACTIVITY_SLUG"); v != "" {
		cfg.PushWard.ActivitySlug = v
	}
	if v := os.Getenv("PUSHWARD_POLL_IDLE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("parsing PUSHWARD_POLL_IDLE: %w", err)
		}
		cfg.Polling.IdleInterval = d
	}
	if v := os.Getenv("PUSHWARD_POLL_ACTIVE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("parsing PUSHWARD_POLL_ACTIVE: %w", err)
		}
		cfg.Polling.ActiveInterval = d
	}

	// Validation
	if cfg.GitHub.Token == "" {
		return nil, fmt.Errorf("github.token is required (set PUSHWARD_GITHUB_TOKEN)")
	}
	if len(cfg.GitHub.Repos) == 0 {
		return nil, fmt.Errorf("github.repos is required (set PUSHWARD_GITHUB_REPOS)")
	}
	if cfg.PushWard.URL == "" {
		return nil, fmt.Errorf("pushward.url is required (set PUSHWARD_URL)")
	}
	if cfg.PushWard.APIKey == "" {
		return nil, fmt.Errorf("pushward.api_key is required (set PUSHWARD_API_KEY)")
	}
	if cfg.PushWard.ActivitySlug == "" {
		return nil, fmt.Errorf("pushward.activity_slug is required (set PUSHWARD_ACTIVITY_SLUG)")
	}

	return cfg, nil
}
