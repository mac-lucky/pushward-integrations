package config

import (
	"fmt"
	"net/url"
	"os"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

type Config struct {
	Forgejo  ForgejoConfig               `yaml:"forgejo"`
	PushWard sharedconfig.PushWardConfig `yaml:"pushward"`
	Polling  PollingConfig               `yaml:"polling"`
	Render   sharedconfig.RenderConfig   `yaml:"render"`
}

type ForgejoConfig struct {
	// URL is the instance root, e.g. https://git.example.com. Required: unlike
	// GitHub there is no single well-known host, and /api/v1 is appended by the
	// client. A value that already ends in /api/v1 is accepted too.
	URL string `yaml:"url"`
	// Token is a Forgejo API token with read access to the repos to watch. It
	// does NOT need the read:organization scope - discovery tolerates the 403
	// that scope's absence produces and falls back to the user endpoint.
	Token string `yaml:"token"`
	// Owner auto-discovers repos. When it matches the token's own login, every
	// repo the token can reach is discovered, including repos owned by others.
	Owner string `yaml:"owner"`
	// Repos are watched in addition to whatever Owner discovers.
	Repos []string `yaml:"repos"`
	// Timeout bounds one API call. Self-hosted instances sit on a LAN rather than
	// behind a CDN, so this is tunable where the github bridge hardcodes it.
	Timeout time.Duration `yaml:"timeout"`
}

// defaultActiveInterval is the gap between updates to a run already in flight
// when polling.interval is not set. Kept well under the idle default because the
// two tiers cost very different amounts: this one is a request per *running* run,
// while the idle tier is a request per *watched repo*.
const defaultActiveInterval = 15 * time.Second

type PollingConfig struct {
	// IdleInterval is how often every watched repo is checked for a run that has
	// just started - one request per repo per pass, so on an instance where owner
	// discovery finds a lot of repos this is the bridge's whole idle request rate.
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
		Forgejo: ForgejoConfig{
			Timeout: 15 * time.Second,
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
	if v := os.Getenv("PUSHWARD_FORGEJO_URL"); v != "" {
		cfg.Forgejo.URL = v
	}
	if v := os.Getenv("PUSHWARD_FORGEJO_TOKEN"); v != "" {
		cfg.Forgejo.Token = v
	}
	if v := os.Getenv("PUSHWARD_FORGEJO_OWNER"); v != "" {
		cfg.Forgejo.Owner = v
	}
	if v := os.Getenv("PUSHWARD_FORGEJO_REPOS"); v != "" {
		cfg.Forgejo.Repos = sharedconfig.SplitList(v)
	}
	if err := sharedconfig.EnvDuration("PUSHWARD_FORGEJO_TIMEOUT", &cfg.Forgejo.Timeout); err != nil {
		return nil, err
	}
	if err := sharedconfig.EnvDuration("PUSHWARD_POLL_IDLE", &cfg.Polling.IdleInterval); err != nil {
		return nil, err
	}
	if err := sharedconfig.EnvDuration("PUSHWARD_POLL_INTERVAL", &cfg.Polling.Interval); err != nil {
		return nil, err
	}
	if err := cfg.Render.ApplyEnvOverrides("PUSHWARD_FORGEJO"); err != nil {
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
	if cfg.Forgejo.URL == "" {
		return nil, fmt.Errorf("forgejo.url is required (set PUSHWARD_FORGEJO_URL)")
	}
	if err := validateURL(cfg.Forgejo.URL); err != nil {
		return nil, err
	}
	if cfg.Forgejo.Token == "" {
		return nil, fmt.Errorf("forgejo.token is required (set PUSHWARD_FORGEJO_TOKEN)")
	}
	if len(cfg.Forgejo.Repos) == 0 && cfg.Forgejo.Owner == "" {
		return nil, fmt.Errorf("forgejo.repos or forgejo.owner is required (set PUSHWARD_FORGEJO_REPOS or PUSHWARD_FORGEJO_OWNER)")
	}
	if cfg.Forgejo.Timeout <= 0 {
		return nil, fmt.Errorf("forgejo.timeout must be positive, got %s", cfg.Forgejo.Timeout)
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

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("forgejo.url is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("forgejo.url must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("forgejo.url must include a host, got %q", raw)
	}
	return nil
}
