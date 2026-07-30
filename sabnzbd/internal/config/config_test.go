package config

import (
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
	return filepath.Join(t.TempDir(), "absent.yml")
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
