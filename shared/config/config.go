package config

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// PushWardConfig holds the common PushWard API settings shared by all integrations.
type PushWardConfig struct {
	URL            string        `yaml:"url"`
	APIKey         string        `yaml:"api_key"`
	Priority       int           `yaml:"priority"`
	CleanupDelay   time.Duration `yaml:"cleanup_delay"`
	StaleTimeout   time.Duration `yaml:"stale_timeout"`
	EndDelay       time.Duration `yaml:"end_delay"`
	EndDisplayTime time.Duration `yaml:"end_display_time"`

	// DismissalDelay maps to the server's dismissal_ttl: how long an ENDED
	// activity stays on the Lock Screen, decoupled from CleanupDelay, which
	// governs deletion. Nil leaves the server default (removal follows
	// ended_ttl, capped at 4h). A pointer because 0 means "remove immediately".
	DismissalDelay *time.Duration `yaml:"dismissal_delay"`
}

// DefaultPushWardConfig is the shipped default for a bridge that closes its
// activities in two phases: a final ONGOING update lands the completion content
// on the Dynamic Island, then ENDED follows after EndDisplayTime.
//
// URL and APIKey stay empty. Neither has a sane default and Validate requires
// both, so a bridge that forgot to configure them fails at startup rather than
// pushing at some placeholder host.
//
// A bridge that diverges assigns over the field it differs on rather than
// restating the whole set, which keeps the divergence and its reason together.
// One caller does not use this at all: the grafana bridge ends in a single shot
// and leaves EndDelay and EndDisplayTime unset on purpose.
func DefaultPushWardConfig() PushWardConfig {
	return PushWardConfig{
		Priority:       1,
		CleanupDelay:   15 * time.Minute,
		StaleTimeout:   30 * time.Minute,
		EndDelay:       5 * time.Second,
		EndDisplayTime: 4 * time.Second,
	}
}

// ServerConfig holds the HTTP server settings for webhook-based integrations.
type ServerConfig struct {
	Address        string `yaml:"address"`
	MetricsAddress string `yaml:"metrics_address"` // Listen address for the internal metrics server (default :9090).
}

// LoadYAML reads a YAML config file into target. Missing files are tolerated (ENOENT).
func LoadYAML(path string, target any) error {
	data, err := os.ReadFile(path) // #nosec G304 -- path comes from CLI flags, not user input
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading config: %w", err)
	}
	if os.IsNotExist(err) {
		slog.Info("config file not found, using defaults and env vars", "path", path)
		return nil
	}
	if err := yaml.Unmarshal(data, target); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	return nil
}

// ApplyEnvOverrides applies PUSHWARD_* environment variable overrides.
func (c *PushWardConfig) ApplyEnvOverrides() error {
	if v := os.Getenv("PUSHWARD_URL"); v != "" {
		c.URL = v
	}
	if v := os.Getenv("PUSHWARD_API_KEY"); v != "" {
		c.APIKey = v
	}
	if err := EnvInt("PUSHWARD_PRIORITY", &c.Priority); err != nil {
		return err
	}
	if err := EnvDuration("PUSHWARD_CLEANUP_DELAY", &c.CleanupDelay); err != nil {
		return err
	}
	if err := EnvDuration("PUSHWARD_STALE_TIMEOUT", &c.StaleTimeout); err != nil {
		return err
	}
	if err := EnvDuration("PUSHWARD_END_DELAY", &c.EndDelay); err != nil {
		return err
	}
	if err := EnvDuration("PUSHWARD_END_DISPLAY_TIME", &c.EndDisplayTime); err != nil {
		return err
	}
	return EnvDurationPtr("PUSHWARD_DISMISSAL_DELAY", &c.DismissalDelay)
}

// Validate checks that required fields are set and priority is in range.
func (c *PushWardConfig) Validate() error {
	if c.URL == "" {
		return fmt.Errorf("pushward.url is required (set PUSHWARD_URL)")
	}
	if c.APIKey == "" {
		return fmt.Errorf("pushward.api_key is required (set PUSHWARD_API_KEY)")
	}
	if c.Priority < 0 || c.Priority > 10 {
		return fmt.Errorf("pushward.priority must be 0-10 (got %d)", c.Priority)
	}
	// The server takes both TTLs as 1-2592000 seconds. Out-of-range values are
	// only rejected once the first activity is created, which strands the bridge
	// looking healthy while every push fails. Zero stays legal: it omits the
	// field and lets the server default apply.
	if c.CleanupDelay < 0 || c.CleanupDelay > 720*time.Hour {
		return fmt.Errorf("pushward.cleanup_delay must be 0-720h (got %v)", c.CleanupDelay)
	}
	if c.StaleTimeout < 0 || c.StaleTimeout > 720*time.Hour {
		return fmt.Errorf("pushward.stale_timeout must be 0-720h (got %v)", c.StaleTimeout)
	}
	// dismissal_ttl has a tighter ceiling than the other two: 4h is the iOS
	// limit, not a server policy, so there is nothing to raise it to.
	maxDismissal := time.Duration(pushward.DismissalTTLMax) * time.Second
	if c.DismissalDelay != nil && (*c.DismissalDelay < 0 || *c.DismissalDelay > maxDismissal) {
		return fmt.Errorf("pushward.dismissal_delay must be 0-4h (got %v)", *c.DismissalDelay)
	}
	return nil
}

// CreateOptions turns the configured dismissal delay into CreateActivity
// options, so a call site can splat it without a nil check.
func (c PushWardConfig) CreateOptions() []pushward.CreateOption {
	return pushward.DismissalTTLOptions(c.DismissalDelay)
}

// ApplyEnvOverrides applies PUSHWARD_SERVER_* environment variable overrides.
func (c *ServerConfig) ApplyEnvOverrides() {
	if v := os.Getenv("PUSHWARD_SERVER_ADDRESS"); v != "" {
		c.Address = v
	}
	if v := os.Getenv("PUSHWARD_SERVER_METRICS_ADDRESS"); v != "" {
		c.MetricsAddress = v
	}
}
