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
	t.Setenv("PUSHWARD_BAMBULAB_HOST", "192.0.2.10")
	t.Setenv("PUSHWARD_BAMBULAB_ACCESS_CODE", "12345678")
	t.Setenv("PUSHWARD_BAMBULAB_SERIAL", "01P00A000000000")
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
	if cfg.Polling.UpdateInterval != 5*time.Second {
		t.Errorf("polling.update_interval = %s, want 5s", cfg.Polling.UpdateInterval)
	}
	if cfg.PushWard.Priority != 1 {
		t.Errorf("pushward.priority = %d, want 1", cfg.PushWard.Priority)
	}
	if cfg.PushWard.CleanupDelay != 15*time.Minute {
		t.Errorf("pushward.cleanup_delay = %s, want 15m", cfg.PushWard.CleanupDelay)
	}
	// Sixty minutes, not the shared thirty: a print is a long job that reports
	// over MQTT, and a half-hour TTL would evict the card mid-print.
	if cfg.PushWard.StaleTimeout != 60*time.Minute {
		t.Errorf("pushward.stale_timeout = %s, want 60m", cfg.PushWard.StaleTimeout)
	}
	if cfg.PushWard.EndDelay != 5*time.Second {
		t.Errorf("pushward.end_delay = %s, want 5s", cfg.PushWard.EndDelay)
	}
	if cfg.PushWard.EndDisplayTime != 4*time.Second {
		t.Errorf("pushward.end_display_time = %s, want 4s", cfg.PushWard.EndDisplayTime)
	}
}

// The fingerprint is what turns "accept any self-signed cert" into pinned
// verification, so an override that silently stopped arriving would drop the pinning
// with nothing else failing.
func TestLoadCertFingerprintOverride(t *testing.T) {
	path := baseEnv(t)
	const fingerprint = "aa:bb:cc:dd:ee:ff:00:11"
	t.Setenv("PUSHWARD_BAMBULAB_CERT_FINGERPRINT", fingerprint)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BambuLab.TLS.CertFingerprintSHA256 != fingerprint {
		t.Errorf("cert_fingerprint_sha256 = %q, want %q", cfg.BambuLab.TLS.CertFingerprintSHA256, fingerprint)
	}
}

// The yaml tag is the contract for a chart that mounts a config file instead of
// setting the env var, and env-beats-YAML is the documented precedence.
func TestLoadUpdateIntervalFromYAML(t *testing.T) {
	baseEnv(t)
	path := writeConfig(t, "polling:\n  update_interval: 12s\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Polling.UpdateInterval != 12*time.Second {
		t.Errorf("polling.update_interval = %s, want the 12s from the file", cfg.Polling.UpdateInterval)
	}

	t.Setenv("PUSHWARD_POLL_INTERVAL", "20s")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Polling.UpdateInterval != 20*time.Second {
		t.Errorf("polling.update_interval = %s, want the env var to beat the file", cfg.Polling.UpdateInterval)
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
