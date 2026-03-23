package config

import (
	"fmt"
	"os"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

type Config struct {
	BambuLab BambuLabConfig             `yaml:"bambulab"`
	PushWard sharedconfig.PushWardConfig `yaml:"pushward"`
	Polling  PollingConfig              `yaml:"polling"`
}

type BambuLabConfig struct {
	Host       string    `yaml:"host"`
	AccessCode string    `yaml:"access_code"`
	Serial     string    `yaml:"serial"`
	TLS        TLSConfig `yaml:"tls"`
}

type TLSConfig struct {
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

type PollingConfig struct {
	UpdateInterval time.Duration `yaml:"update_interval"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		BambuLab: BambuLabConfig{
			TLS: TLSConfig{
				InsecureSkipVerify: false,
			},
		},
		PushWard: sharedconfig.PushWardConfig{
			Priority:       1,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   60 * time.Minute,
			EndDelay:       5 * time.Second,
			EndDisplayTime: 4 * time.Second,
		},
		Polling: PollingConfig{
			UpdateInterval: 5 * time.Second,
		},
	}

	if err := sharedconfig.LoadYAML(path, cfg); err != nil {
		return nil, err
	}

	// Integration-specific env overrides
	if v := os.Getenv("PUSHWARD_BAMBULAB_HOST"); v != "" {
		cfg.BambuLab.Host = v
	}
	if v := os.Getenv("PUSHWARD_BAMBULAB_ACCESS_CODE"); v != "" {
		cfg.BambuLab.AccessCode = v
	}
	if v := os.Getenv("PUSHWARD_BAMBULAB_SERIAL"); v != "" {
		cfg.BambuLab.Serial = v
	}
	if v := os.Getenv("PUSHWARD_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("parsing PUSHWARD_POLL_INTERVAL: %w", err)
		}
		cfg.Polling.UpdateInterval = d
	}

	// Shared PushWard env overrides
	if err := cfg.PushWard.ApplyEnvOverrides(); err != nil {
		return nil, err
	}

	// Validation
	if cfg.BambuLab.Host == "" {
		return nil, fmt.Errorf("bambulab.host is required (set PUSHWARD_BAMBULAB_HOST)")
	}
	if cfg.BambuLab.AccessCode == "" {
		return nil, fmt.Errorf("bambulab.access_code is required (set PUSHWARD_BAMBULAB_ACCESS_CODE)")
	}
	if cfg.BambuLab.Serial == "" {
		return nil, fmt.Errorf("bambulab.serial is required (set PUSHWARD_BAMBULAB_SERIAL)")
	}
	if err := cfg.PushWard.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
