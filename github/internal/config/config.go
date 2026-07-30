package config

import (
	"fmt"
	"os"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

type Config struct {
	GitHub   GitHubConfig                `yaml:"github"`
	PushWard sharedconfig.PushWardConfig `yaml:"pushward"`
	Polling  PollingConfig               `yaml:"polling"`
	Render   sharedconfig.RenderConfig   `yaml:"render"`
}

type GitHubConfig struct {
	Token string   `yaml:"token"`
	Owner string   `yaml:"owner"`
	Repos []string `yaml:"repos"`
}

// defaultActiveInterval is the gap between updates to a run already in flight
// when polling.interval is not set. Kept well under the idle default because the
// two tiers cost very different amounts: this one is a request per *running* run,
// while the idle tier is a request per *watched repo*.
const defaultActiveInterval = 15 * time.Second

type PollingConfig struct {
	// IdleInterval is how often every watched repo is checked for a run that has
	// just started. One request per repo per pass, so with a large owner this is
	// the whole idle request rate: 44 repos at 30s is 5,280 requests an hour
	// against GitHub's 5,000, which is how this knob gets a bridge throttled.
	IdleInterval time.Duration `yaml:"idle_interval"`
	// Interval is how often a run already being tracked is advanced - what someone
	// watching the card sees. One request per run in flight, so it stays cheap no
	// matter how many repos are watched. Defaults to the smaller of idle_interval
	// and 15s, so lowering idle_interval alone never leaves this slower than it.
	Interval time.Duration `yaml:"interval"`
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
		Render: sharedconfig.DefaultRenderConfig(),
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
		cfg.GitHub.Repos = sharedconfig.SplitList(v)
	}
	if err := sharedconfig.EnvDuration("PUSHWARD_POLL_IDLE", &cfg.Polling.IdleInterval); err != nil {
		return nil, err
	}
	if err := sharedconfig.EnvDuration("PUSHWARD_POLL_INTERVAL", &cfg.Polling.Interval); err != nil {
		return nil, err
	}
	if err := cfg.Render.ApplyEnvOverrides("PUSHWARD_GITHUB"); err != nil {
		return nil, err
	}

	// After the overrides, so an operator who lowered idle_interval to get faster
	// cards is not left with an active tier slower than the idle one. Only an unset
	// value inherits: a negative one is a mistake and falls through to validation
	// rather than being quietly turned into something workable.
	if cfg.Polling.Interval == 0 {
		cfg.Polling.Interval = min(cfg.Polling.IdleInterval, defaultActiveInterval)
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
	if cfg.Polling.IdleInterval <= 0 {
		return nil, fmt.Errorf("polling.idle_interval must be positive, got %s", cfg.Polling.IdleInterval)
	}
	if cfg.Polling.Interval <= 0 {
		return nil, fmt.Errorf("polling.interval must be positive, got %s", cfg.Polling.Interval)
	}
	if cfg.Polling.Interval > cfg.Polling.IdleInterval {
		return nil, fmt.Errorf("polling.interval (%s) must not exceed polling.idle_interval (%s): the active tier cannot be slower than the idle one",
			cfg.Polling.Interval, cfg.Polling.IdleInterval)
	}

	// Shared validation
	if err := cfg.PushWard.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
