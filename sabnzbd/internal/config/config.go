package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	SABnzbd  SABnzbdConfig  `yaml:"sabnzbd"`
	PushWard PushWardConfig `yaml:"pushward"`
	Polling  PollingConfig  `yaml:"polling"`
}

type ServerConfig struct {
	Address string `yaml:"address"`
}

type SABnzbdConfig struct {
	URL    string `yaml:"url"`
	APIKey string `yaml:"api_key"`
}

type PushWardConfig struct {
	URL          string        `yaml:"url"`
	APIKey       string        `yaml:"api_key"`
	Priority     int           `yaml:"priority"`
	CleanupDelay time.Duration `yaml:"cleanup_delay"`
}

type PollingConfig struct {
	Interval time.Duration `yaml:"interval"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Address: ":8090",
		},
		PushWard: PushWardConfig{
			Priority:     1,
			CleanupDelay: 15 * time.Minute,
		},
		Polling: PollingConfig{
			Interval: 1 * time.Second,
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
	if v := os.Getenv("PUSHWARD_SABNZBD_URL"); v != "" {
		cfg.SABnzbd.URL = v
	}
	if v := os.Getenv("PUSHWARD_SABNZBD_API_KEY"); v != "" {
		cfg.SABnzbd.APIKey = v
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
	if v := os.Getenv("PUSHWARD_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("parsing PUSHWARD_POLL_INTERVAL: %w", err)
		}
		cfg.Polling.Interval = d
	}

	// Validation
	if cfg.SABnzbd.URL == "" {
		return nil, fmt.Errorf("sabnzbd.url is required (set PUSHWARD_SABNZBD_URL)")
	}
	if cfg.SABnzbd.APIKey == "" {
		return nil, fmt.Errorf("sabnzbd.api_key is required (set PUSHWARD_SABNZBD_API_KEY)")
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
