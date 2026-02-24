package config

import (
	"fmt"
	"os"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-docker/shared/config"
)

type Config struct {
	Server   sharedconfig.ServerConfig   `yaml:"server"`
	SABnzbd  SABnzbdConfig              `yaml:"sabnzbd"`
	PushWard sharedconfig.PushWardConfig `yaml:"pushward"`
	Polling  PollingConfig              `yaml:"polling"`
}

type SABnzbdConfig struct {
	URL    string `yaml:"url"`
	APIKey string `yaml:"api_key"`
}

type PollingConfig struct {
	Interval time.Duration `yaml:"interval"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: sharedconfig.ServerConfig{
			Address: ":8090",
		},
		PushWard: sharedconfig.PushWardConfig{
			Priority:       1,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       5 * time.Second,
			EndDisplayTime: 4 * time.Second,
		},
		Polling: PollingConfig{
			Interval: 1 * time.Second,
		},
	}

	if err := sharedconfig.LoadYAML(path, cfg); err != nil {
		return nil, err
	}

	// Integration-specific env overrides
	cfg.Server.ApplyEnvOverrides()
	if v := os.Getenv("PUSHWARD_SABNZBD_URL"); v != "" {
		cfg.SABnzbd.URL = v
	}
	if v := os.Getenv("PUSHWARD_SABNZBD_API_KEY"); v != "" {
		cfg.SABnzbd.APIKey = v
	}
	if v := os.Getenv("PUSHWARD_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("parsing PUSHWARD_POLL_INTERVAL: %w", err)
		}
		cfg.Polling.Interval = d
	}

	// Shared PushWard env overrides
	if err := cfg.PushWard.ApplyEnvOverrides(); err != nil {
		return nil, err
	}

	// Integration-specific validation
	if cfg.SABnzbd.URL == "" {
		return nil, fmt.Errorf("sabnzbd.url is required (set PUSHWARD_SABNZBD_URL)")
	}
	if cfg.SABnzbd.APIKey == "" {
		return nil, fmt.Errorf("sabnzbd.api_key is required (set PUSHWARD_SABNZBD_API_KEY)")
	}

	// Shared validation
	if err := cfg.PushWard.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
