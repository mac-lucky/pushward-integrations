package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

type Config struct {
	GitHub   GitHubConfig                `yaml:"github"`
	PushWard sharedconfig.PushWardConfig `yaml:"pushward"`
	Polling  PollingConfig               `yaml:"polling"`
	Render   RenderConfig                `yaml:"render"`
}

// RenderConfig gates the step-pill fields. The two pill fields default to off,
// which reproduces the payload the bridge sent before they existed.
type RenderConfig struct {
	StepColors  bool `yaml:"step_colors"`
	StepWeights bool `yaml:"step_weights"`
	// LiveProgress lets iOS animate the current step's pill and count its ETA
	// down between polls, anchored to how long that job group ran in the prior
	// run. Unlike the pill fields it defaults on: the anchors are additive, so a
	// client that does not understand them renders the same bar as before.
	LiveProgress bool `yaml:"live_progress"`
}

type GitHubConfig struct {
	Token string   `yaml:"token"`
	Owner string   `yaml:"owner"`
	Repos []string `yaml:"repos"`
}

type PollingConfig struct {
	IdleInterval time.Duration `yaml:"idle_interval"`
}

// envBool applies a boolean environment override to dst, leaving it untouched
// when the variable is unset or empty. An unparseable value is an error rather
// than a silent default: a flag that defaults on would otherwise stay on for
// anyone who wrote "yes" or "enabled" and believed they had turned it off.
func envBool(name string, dst *bool) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", name, err)
	}
	*dst = b
	return nil
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		PushWard: sharedconfig.PushWardConfig{
			Priority:       1,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       5 * time.Second,
			EndDisplayTime: 4 * time.Second,
		},
		Polling: PollingConfig{
			IdleInterval: 60 * time.Second,
		},
		Render: RenderConfig{
			LiveProgress: true,
		},
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
		cfg.GitHub.Repos = strings.Split(v, ",")
	}
	if v := os.Getenv("PUSHWARD_POLL_IDLE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("parsing PUSHWARD_POLL_IDLE: %w", err)
		}
		cfg.Polling.IdleInterval = d
	}
	if err := envBool("PUSHWARD_GITHUB_STEP_COLORS", &cfg.Render.StepColors); err != nil {
		return nil, err
	}
	if err := envBool("PUSHWARD_GITHUB_STEP_WEIGHTS", &cfg.Render.StepWeights); err != nil {
		return nil, err
	}
	if err := envBool("PUSHWARD_GITHUB_LIVE_PROGRESS", &cfg.Render.LiveProgress); err != nil {
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

	// Shared validation
	if err := cfg.PushWard.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
