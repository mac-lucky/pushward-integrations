package config

import (
	"fmt"
	"os"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

type Config struct {
	MQTT     MQTTConfig                 `yaml:"mqtt"`
	Rules    []RuleConfig               `yaml:"rules"`
	PushWard sharedconfig.PushWardConfig `yaml:"pushward"`
	Polling  PollingConfig              `yaml:"polling"`
}

type MQTTConfig struct {
	Broker   string    `yaml:"broker"`
	Username string    `yaml:"username"`
	Password string    `yaml:"password"`
	ClientID string    `yaml:"client_id"`
	TLS      TLSConfig `yaml:"tls"`
}

type TLSConfig struct {
	Enabled            bool   `yaml:"enabled"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	CACert             string `yaml:"ca_cert"`
	ClientCert         string `yaml:"client_cert"`
	ClientKey          string `yaml:"client_key"`
}

type PollingConfig struct {
	UpdateInterval time.Duration `yaml:"update_interval"`
}

type RuleConfig struct {
	Name              string            `yaml:"name"`
	Topic             string            `yaml:"topic"`
	Slug              string            `yaml:"slug"`
	Template          string            `yaml:"template"`
	Priority          *int              `yaml:"priority"`
	Lifecycle         string            `yaml:"lifecycle"`
	StateField        string            `yaml:"state_field"`
	StateMap          map[string]string `yaml:"state_map"`
	InactivityTimeout time.Duration     `yaml:"inactivity_timeout"`
	Content           ContentMapping    `yaml:"content"`
}

type ContentMapping struct {
	State         string     `yaml:"state"`
	Subtitle      string     `yaml:"subtitle"`
	Progress      string     `yaml:"progress"`
	Icon          FieldOrMap `yaml:"icon"`
	AccentColor   FieldOrMap `yaml:"accent_color"`
	RemainingTime string     `yaml:"remaining_time"`
	CurrentStep   string     `yaml:"current_step"`
	TotalSteps    string     `yaml:"total_steps"`
	URL           string     `yaml:"url"`
	SecondaryURL  string     `yaml:"secondary_url"`
	Severity      string     `yaml:"severity"`
}

type FieldOrMap struct {
	Default string                       `yaml:"default"`
	Map     map[string]map[string]string `yaml:"map"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		MQTT: MQTTConfig{
			ClientID: "pushward-mqtt",
		},
		PushWard: sharedconfig.PushWardConfig{
			Priority:       1,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   30 * time.Minute,
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

	// MQTT env overrides
	if v := os.Getenv("PUSHWARD_MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if v := os.Getenv("PUSHWARD_MQTT_USERNAME"); v != "" {
		cfg.MQTT.Username = v
	}
	if v := os.Getenv("PUSHWARD_MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	if v := os.Getenv("PUSHWARD_MQTT_CLIENT_ID"); v != "" {
		cfg.MQTT.ClientID = v
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
	if cfg.MQTT.Broker == "" {
		return nil, fmt.Errorf("mqtt.broker is required (set PUSHWARD_MQTT_BROKER)")
	}
	if len(cfg.Rules) == 0 {
		return nil, fmt.Errorf("at least one rule is required")
	}
	for i, r := range cfg.Rules {
		if r.Name == "" {
			return nil, fmt.Errorf("rules[%d].name is required", i)
		}
		if r.Topic == "" {
			return nil, fmt.Errorf("rules[%d].topic is required", i)
		}
		if r.Slug == "" {
			return nil, fmt.Errorf("rules[%d].slug is required", i)
		}
		if r.Lifecycle == "" {
			return nil, fmt.Errorf("rules[%d].lifecycle is required (field or presence)", i)
		}
		if r.Lifecycle != "field" && r.Lifecycle != "presence" {
			return nil, fmt.Errorf("rules[%d].lifecycle must be 'field' or 'presence' (got %q)", i, r.Lifecycle)
		}
		if r.Lifecycle == "field" {
			if r.StateField == "" {
				return nil, fmt.Errorf("rules[%d].state_field is required for field lifecycle", i)
			}
			if len(r.StateMap) == 0 {
				return nil, fmt.Errorf("rules[%d].state_map is required for field lifecycle", i)
			}
		}
		if r.Lifecycle == "presence" {
			if r.InactivityTimeout <= 0 {
				return nil, fmt.Errorf("rules[%d].inactivity_timeout must be > 0 for presence lifecycle", i)
			}
		}
		// Apply defaults
		if cfg.Rules[i].Template == "" {
			cfg.Rules[i].Template = "generic"
		}
	}
	if err := cfg.PushWard.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
