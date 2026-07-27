package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

type Config struct {
	Backrest BackrestConfig              `yaml:"backrest"`
	PushWard sharedconfig.PushWardConfig `yaml:"pushward"`
	Polling  PollingConfig               `yaml:"polling"`
	Render   RenderConfig                `yaml:"render"`
}

// BackrestConfig points the bridge at a Backrest instance. Backrest's auth
// middleware accepts HTTP Basic or a bearer JWT and passes everything through
// when auth is disabled, so leaving all three credential fields empty is a
// supported setup rather than an incomplete one.
type BackrestConfig struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	Token    string `yaml:"token"`
	// Timeout bounds a single API call. Repo checks can hold the server for a
	// while, but GetOperations itself is a local read and should be quick.
	Timeout time.Duration `yaml:"timeout"`
}

type PollingConfig struct {
	// Interval is how often the bridge polls while an operation is in flight.
	Interval time.Duration `yaml:"interval"`
	// IdleInterval is how often it polls when nothing is running. Backrest is
	// usually a local service, so the cost of a tick is small, but there is no
	// reason to ask several times a second whether a nightly backup started.
	IdleInterval time.Duration `yaml:"idle_interval"`
	// LastN is how many recent operations to request each tick. It has to be
	// wide enough that the running operation is still inside the window after
	// the hook and stats rows a backup generates land on top of it.
	LastN int64 `yaml:"last_n"`
}

// RenderConfig gates the optional presentation features.
type RenderConfig struct {
	// LiveProgress lets iOS animate the bar and count the ETA down between
	// polls instead of stepping once per tick. It defaults on: the anchors are
	// additive, so a client that does not understand them renders exactly the
	// bar it rendered before.
	LiveProgress bool `yaml:"live_progress"`
	// Logs renders prune and check as a log view of their command output.
	// Defaults on; turning it off falls back to a plain state line and skips
	// the GetLogs call entirely.
	Logs bool `yaml:"logs"`
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

func envDuration(name string, dst *time.Duration) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", name, err)
	}
	*dst = d
	return nil
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Backrest: BackrestConfig{
			Timeout: 15 * time.Second,
		},
		PushWard: sharedconfig.PushWardConfig{
			Priority:       1,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       5 * time.Second,
			EndDisplayTime: 4 * time.Second,
		},
		Polling: PollingConfig{
			Interval:     5 * time.Second,
			IdleInterval: 30 * time.Second,
			LastN:        50,
		},
		Render: RenderConfig{
			LiveProgress: true,
			Logs:         true,
		},
	}

	if err := sharedconfig.LoadYAML(path, cfg); err != nil {
		return nil, err
	}

	// Integration-specific env overrides
	if v := os.Getenv("PUSHWARD_BACKREST_URL"); v != "" {
		cfg.Backrest.URL = v
	}
	if v := os.Getenv("PUSHWARD_BACKREST_USERNAME"); v != "" {
		cfg.Backrest.Username = v
	}
	if v := os.Getenv("PUSHWARD_BACKREST_PASSWORD"); v != "" {
		cfg.Backrest.Password = v
	}
	if v := os.Getenv("PUSHWARD_BACKREST_TOKEN"); v != "" {
		cfg.Backrest.Token = v
	}
	if err := envDuration("PUSHWARD_BACKREST_TIMEOUT", &cfg.Backrest.Timeout); err != nil {
		return nil, err
	}
	if err := envDuration("PUSHWARD_POLL_INTERVAL", &cfg.Polling.Interval); err != nil {
		return nil, err
	}
	if err := envDuration("PUSHWARD_POLL_IDLE", &cfg.Polling.IdleInterval); err != nil {
		return nil, err
	}
	if v := os.Getenv("PUSHWARD_BACKREST_LAST_N"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing PUSHWARD_BACKREST_LAST_N: %w", err)
		}
		cfg.Polling.LastN = n
	}
	if err := envBool("PUSHWARD_BACKREST_LIVE_PROGRESS", &cfg.Render.LiveProgress); err != nil {
		return nil, err
	}
	if err := envBool("PUSHWARD_BACKREST_LOGS", &cfg.Render.Logs); err != nil {
		return nil, err
	}

	// Shared PushWard env overrides
	if err := cfg.PushWard.ApplyEnvOverrides(); err != nil {
		return nil, err
	}

	// Integration-specific validation
	if cfg.Backrest.URL == "" {
		return nil, fmt.Errorf("backrest.url is required (set PUSHWARD_BACKREST_URL)")
	}
	if cfg.Polling.Interval <= 0 {
		return nil, fmt.Errorf("polling.interval must be positive")
	}
	if cfg.Polling.IdleInterval <= 0 {
		return nil, fmt.Errorf("polling.idle_interval must be positive")
	}
	if cfg.Polling.LastN <= 0 {
		return nil, fmt.Errorf("polling.last_n must be positive")
	}

	// Shared validation
	if err := cfg.PushWard.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
