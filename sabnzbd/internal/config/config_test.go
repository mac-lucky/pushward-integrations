package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// baseEnv is the minimum that makes Load succeed, so a test can set exactly the
// one variable it is about. The config file is deliberately absent: the whole
// bridge is configurable from the environment, which is how it runs in a container.
func baseEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("PUSHWARD_SABNZBD_URL", "http://sabnzbd:8080")
	t.Setenv("PUSHWARD_SABNZBD_API_KEY", "sab-key")
	t.Setenv("PUSHWARD_URL", "https://api.pushward.app")
	t.Setenv("PUSHWARD_API_KEY", "hlk_test")
	// Cleared, not assumed absent: this name is shared across six bridges, so an
	// exported value in the developer's shell would fail the default assertions for an
	// unrelated reason. Tests that want a value set it after calling this.
	t.Setenv("PUSHWARD_POLL_INTERVAL", "")
	return filepath.Join(t.TempDir(), "absent.yml")
}

// writeConfig writes a config file next to the env-only path baseEnv returns, so the
// YAML branch of Load gets exercised too.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(baseEnv(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Polling.Interval != 5*time.Second {
		t.Errorf("polling.interval = %s, want 5s", cfg.Polling.Interval)
	}
	if cfg.Server.Address != ":8090" {
		t.Errorf("server.address = %q, want :8090", cfg.Server.Address)
	}
	if cfg.SABnzbd.Template != "generic" {
		t.Errorf("template = %q, want generic", cfg.SABnzbd.Template)
	}
	// The plain shared activity tunables, with no per-bridge divergence.
	if cfg.PushWard.Priority != 1 {
		t.Errorf("pushward.priority = %d, want 1", cfg.PushWard.Priority)
	}
	if cfg.PushWard.CleanupDelay != 15*time.Minute {
		t.Errorf("pushward.cleanup_delay = %s, want 15m", cfg.PushWard.CleanupDelay)
	}
	if cfg.PushWard.StaleTimeout != 30*time.Minute {
		t.Errorf("pushward.stale_timeout = %s, want 30m", cfg.PushWard.StaleTimeout)
	}
	if cfg.PushWard.EndDelay != 5*time.Second {
		t.Errorf("pushward.end_delay = %s, want 5s", cfg.PushWard.EndDelay)
	}
	if cfg.PushWard.EndDisplayTime != 4*time.Second {
		t.Errorf("pushward.end_display_time = %s, want 4s", cfg.PushWard.EndDisplayTime)
	}
}

// The yaml tag is the contract for a chart that mounts a config file instead of
// setting the env var, and env-beats-YAML is the documented precedence.
func TestLoadPollIntervalFromYAML(t *testing.T) {
	baseEnv(t)
	path := writeConfig(t, "polling:\n  interval: 12s\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Polling.Interval != 12*time.Second {
		t.Errorf("polling.interval = %s, want the 12s from the file", cfg.Polling.Interval)
	}

	t.Setenv("PUSHWARD_POLL_INTERVAL", "20s")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Polling.Interval != 20*time.Second {
		t.Errorf("polling.interval = %s, want the env var to beat the file", cfg.Polling.Interval)
	}
}

func TestLoadPollIntervalOverride(t *testing.T) {
	path := baseEnv(t)
	t.Setenv("PUSHWARD_POLL_INTERVAL", "10s")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Polling.Interval != 10*time.Second {
		t.Errorf("polling.interval = %s, want 10s", cfg.Polling.Interval)
	}
}

// A typo in a manifest must not quietly leave the default in place.
func TestLoadRejectsAnUnparseablePollInterval(t *testing.T) {
	path := baseEnv(t)
	t.Setenv("PUSHWARD_POLL_INTERVAL", "10")

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "PUSHWARD_POLL_INTERVAL") {
		t.Errorf("error should name the variable, got %v", err)
	}
}

// Sub-second polling would hammer the SABnzbd API for no visible benefit, so it is
// rejected outright rather than clamped.
func TestLoadRejectsAPollIntervalBelowASecond(t *testing.T) {
	for _, v := range []string{"999ms", "0s", "-1s"} {
		t.Run(v, func(t *testing.T) {
			path := baseEnv(t)
			t.Setenv("PUSHWARD_POLL_INTERVAL", v)

			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected %s to be rejected", v)
			}
			if !strings.Contains(err.Error(), "polling.interval") {
				t.Errorf("error should name the field, got %v", err)
			}
		})
	}
}
