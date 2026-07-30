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
	Polling  sharedconfig.PollingConfig  `yaml:"polling"`
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

func Load(path string) (*Config, error) {
	cfg := &Config{
		PushWard: sharedconfig.DefaultPushWardConfig(),
		Forgejo: ForgejoConfig{
			Timeout: 15 * time.Second,
		},
		Polling: sharedconfig.DefaultPollingConfig(),
		Render:  sharedconfig.DefaultRenderConfig(),
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
	if err := cfg.Polling.ApplyEnvOverrides(); err != nil {
		return nil, err
	}
	if err := cfg.Render.ApplyEnvOverrides("PUSHWARD_FORGEJO"); err != nil {
		return nil, err
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
	// Derive before checking, and only once every override layer above has run: the
	// active tier comes from whatever idle_interval survived them.
	cfg.Polling.ApplyActiveDefault()
	if err := cfg.Polling.Validate(); err != nil {
		return nil, err
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
