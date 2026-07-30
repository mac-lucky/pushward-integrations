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
	t.Setenv("PUSHWARD_BAMBULAB_HOST", "192.0.2.10")
	t.Setenv("PUSHWARD_BAMBULAB_ACCESS_CODE", "12345678")
	t.Setenv("PUSHWARD_BAMBULAB_SERIAL", "01P00A000000000")
	t.Setenv("PUSHWARD_URL", "https://api.pushward.app")
	t.Setenv("PUSHWARD_API_KEY", "hlk_test")
	return filepath.Join(t.TempDir(), "absent.yml")
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(baseEnv(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Polling.UpdateInterval != 5*time.Second {
		t.Errorf("polling.update_interval = %s, want 5s", cfg.Polling.UpdateInterval)
	}
	// The printer's cert is self-signed, but verification is only skipped when an
	// operator asks for it.
	if cfg.BambuLab.TLS.InsecureSkipVerify {
		t.Error("insecure_skip_verify must default off")
	}
}

// The variable is PUSHWARD_POLL_INTERVAL even though the YAML key is
// update_interval, matching every other bridge.
func TestLoadUpdateIntervalOverride(t *testing.T) {
	path := baseEnv(t)
	t.Setenv("PUSHWARD_POLL_INTERVAL", "10s")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Polling.UpdateInterval != 10*time.Second {
		t.Errorf("polling.update_interval = %s, want 10s", cfg.Polling.UpdateInterval)
	}
}

// A typo in a manifest must not quietly leave the default in place.
func TestLoadRejectsAnUnparseableUpdateInterval(t *testing.T) {
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

// The printer pushes MQTT reports of its own; polling faster than 2s buys nothing
// and the debounce is keyed off this value.
func TestLoadRejectsAnUpdateIntervalBelowTwoSeconds(t *testing.T) {
	for _, v := range []string{"1s", "0s", "-1s"} {
		t.Run(v, func(t *testing.T) {
			path := baseEnv(t)
			t.Setenv("PUSHWARD_POLL_INTERVAL", v)

			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected %s to be rejected", v)
			}
			if !strings.Contains(err.Error(), "polling.update_interval") {
				t.Errorf("error should name the field, got %v", err)
			}
		})
	}
}
