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
	Owner string   `yaml:"owner"`
	Repos []string `yaml:"repos"`
}

type PushWardConfig struct {
	URL          string        `yaml:"url"`
	APIKey       string        `yaml:"api_key"`
	Priority     int           `yaml:"priority"`
	CleanupDelay time.Duration `yaml:"cleanup_delay"`
}

type PollingConfig struct {
	IdleInterval   time.Duration `yaml:"idle_interval"`
	ActiveInterval time.Duration `yaml:"active_interval"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		PushWard: PushWardConfig{
			Priority:     1,
			CleanupDelay: 15 * time.Minute,
		},
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
	if v := os.Getenv("PUSHWARD_GITHUB_OWNER"); v != "" {
		cfg.GitHub.Owner = v
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
	if len(cfg.GitHub.Repos) == 0 && cfg.GitHub.Owner == "" {
		return nil, fmt.Errorf("github.repos or github.owner is required (set PUSHWARD_GITHUB_REPOS or PUSHWARD_GITHUB_OWNER)")
	}
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
