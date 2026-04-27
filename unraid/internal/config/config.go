package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

type Config struct {
	Unraid   UnraidConfig                `yaml:"unraid"`
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
			Host:       "localhost",
			Port:       80,
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

	if v := envOr("PUSHWARD_UNRAID_HOST", "UNRAID_HOST"); v != "" {
		cfg.Unraid.Host = v
	}
	if v := envOr("PUSHWARD_UNRAID_PORT", "UNRAID_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("parsing PUSHWARD_UNRAID_PORT: %w", err)
		}
		cfg.Unraid.Port = p
	}
	if v := envOr("PUSHWARD_UNRAID_API_KEY", "UNRAID_API_KEY"); v != "" {
		cfg.Unraid.APIKey = v
	}
	if v := envOr("PUSHWARD_UNRAID_USE_TLS", "UNRAID_USE_TLS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("parsing PUSHWARD_UNRAID_USE_TLS: %w", err)
		}
		cfg.Unraid.UseTLS = b
	}
	if v := envOr("PUSHWARD_UNRAID_SERVER_NAME", "UNRAID_SERVER_NAME"); v != "" {
		cfg.Unraid.ServerName = v
	}

	if err := cfg.PushWard.ApplyEnvOverrides(); err != nil {
		return nil, err
	}

	if cfg.Unraid.Host == "" {
		return nil, fmt.Errorf("unraid.host is required (set PUSHWARD_UNRAID_HOST)")
	}
	if cfg.Unraid.APIKey == "" {
		return nil, fmt.Errorf("unraid.api_key is required (set PUSHWARD_UNRAID_API_KEY)")
	}
	if err := cfg.PushWard.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func envOr(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
