package config

import (
	"fmt"
	"os"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

type Config struct {
	Unraid   UnraidConfig               `yaml:"unraid"`
	PushWard sharedconfig.PushWardConfig `yaml:"pushward"`
}

type UnraidConfig struct {
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	APIKey     string `yaml:"api_key"`
	ServerName string `yaml:"server_name"`
	UseTLS     bool   `yaml:"use_tls"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Unraid: UnraidConfig{
			Port:       3001,
			ServerName: "Unraid",
		},
		PushWard: sharedconfig.PushWardConfig{
			Priority:       2,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   24 * time.Hour,
			EndDelay:       5 * time.Second,
			EndDisplayTime: 4 * time.Second,
		},
	}

	if err := sharedconfig.LoadYAML(path, cfg); err != nil {
		return nil, err
	}

	// Integration-specific env overrides
	if v := os.Getenv("UNRAID_HOST"); v != "" {
		cfg.Unraid.Host = v
	}
	if v := os.Getenv("UNRAID_PORT"); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil {
			cfg.Unraid.Port = p
		}
	}
	if v := os.Getenv("UNRAID_API_KEY"); v != "" {
		cfg.Unraid.APIKey = v
	}

	// Shared PushWard env overrides
	if err := cfg.PushWard.ApplyEnvOverrides(); err != nil {
		return nil, err
	}

	// Validation
	if cfg.Unraid.Host == "" {
		return nil, fmt.Errorf("unraid.host is required (set UNRAID_HOST)")
	}
	if cfg.Unraid.APIKey == "" {
		return nil, fmt.Errorf("unraid.api_key is required (set UNRAID_API_KEY)")
	}
	if err := cfg.PushWard.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
