package config

import (
	"os"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

type Config struct {
	Server   sharedconfig.ServerConfig   `yaml:"server"`
	Radarr   ServiceAuth                 `yaml:"radarr"`
	Sonarr   ServiceAuth                 `yaml:"sonarr"`
	PushWard sharedconfig.PushWardConfig `yaml:"pushward"`
}

type ServiceAuth struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
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
	}

	if err := sharedconfig.LoadYAML(path, cfg); err != nil {
		return nil, err
	}

	// Integration-specific env overrides
	cfg.Server.ApplyEnvOverrides()
	if v := os.Getenv("PUSHWARD_RADARR_USERNAME"); v != "" {
		cfg.Radarr.Username = v
	}
	if v := os.Getenv("PUSHWARD_RADARR_PASSWORD"); v != "" {
		cfg.Radarr.Password = v
	}
	if v := os.Getenv("PUSHWARD_SONARR_USERNAME"); v != "" {
		cfg.Sonarr.Username = v
	}
	if v := os.Getenv("PUSHWARD_SONARR_PASSWORD"); v != "" {
		cfg.Sonarr.Password = v
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
