package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

type Config struct {
	Forgejo  ForgejoConfig               `yaml:"forgejo"`
	PushWard sharedconfig.PushWardConfig `yaml:"pushward"`
	Polling  PollingConfig               `yaml:"polling"`
	Render   RenderConfig                `yaml:"render"`
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

// RenderConfig gates the step-pill fields. The two pill fields default to off,
// matching the github bridge, so an operator opts in to the richer rendering.
type RenderConfig struct {
	StepColors bool `yaml:"step_colors"`
	// StepWeights sizes each pill by how long that group ran in the prior run.
	// Reaching those durations costs one extra tasks lookup per seeded run,
	// because Forgejo's job objects carry no timestamps.
	StepWeights bool `yaml:"step_weights"`
	// LiveProgress lets iOS animate the current step's pill and count its ETA
	// down between polls. Unlike the pill fields it defaults on: the anchors are
	// additive, so a client that does not understand them renders the same bar.
	LiveProgress bool `yaml:"live_progress"`
}

type PollingConfig struct {
	IdleInterval time.Duration `yaml:"idle_interval"`
}

// envBool applies a bool env override, erroring on an unparseable value rather
// than silently falling back to the default - a typo in a manifest should fail
// loudly at startup, not quietly disable a feature.
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
		Render: RenderConfig{
			LiveProgress: true,
		},
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
		cfg.Forgejo.Repos = splitRepos(v)
	}
	if err := envDuration("PUSHWARD_FORGEJO_TIMEOUT", &cfg.Forgejo.Timeout); err != nil {
		return nil, err
	}
	if err := envDuration("PUSHWARD_POLL_IDLE", &cfg.Polling.IdleInterval); err != nil {
		return nil, err
	}
	if err := envBool("PUSHWARD_FORGEJO_STEP_COLORS", &cfg.Render.StepColors); err != nil {
		return nil, err
	}
	if err := envBool("PUSHWARD_FORGEJO_STEP_WEIGHTS", &cfg.Render.StepWeights); err != nil {
		return nil, err
	}
	if err := envBool("PUSHWARD_FORGEJO_LIVE_PROGRESS", &cfg.Render.LiveProgress); err != nil {
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
	if cfg.Polling.IdleInterval <= 0 {
		return nil, fmt.Errorf("polling.idle_interval must be positive, got %s", cfg.Polling.IdleInterval)
	}

	// Shared validation
	if err := cfg.PushWard.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// splitRepos parses the comma-separated repo env var, dropping blanks and
// surrounding whitespace so a trailing comma or a wrapped YAML value does not
// produce an empty repo that fails every poll.
func splitRepos(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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
