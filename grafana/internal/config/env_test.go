package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// durationVars are the four env vars applyEnvOverrides parses as durations. Tests
// clear them rather than assume they are absent: PUSHWARD_POLL_INTERVAL in particular
// is shared across six bridges, so an exported value in the developer's shell would
// otherwise make an assertion pass - or fail - for an unrelated reason.
var durationVars = []string{
	"PUSHWARD_METRICS_TIMEOUT",
	"PUSHWARD_ALERT_CHECK_INTERVAL",
	"PUSHWARD_HISTORY_WINDOW",
	"PUSHWARD_POLL_INTERVAL",
}

func clearDurationVars(t *testing.T) {
	t.Helper()
	for _, name := range durationVars {
		t.Setenv(name, "")
	}
}

// applyEnvOverrides is exercised directly rather than through Load: it takes the
// whole Config, so a test needs no otherwise-valid file or credential set.
func TestApplyEnvOverridesDurations(t *testing.T) {
	clearDurationVars(t)
	t.Setenv("PUSHWARD_METRICS_TIMEOUT", "20s")
	t.Setenv("PUSHWARD_ALERT_CHECK_INTERVAL", "1m")
	t.Setenv("PUSHWARD_HISTORY_WINDOW", "2h")
	t.Setenv("PUSHWARD_POLL_INTERVAL", "45s")

	var cfg Config
	if err := applyEnvOverrides(&cfg); err != nil {
		t.Fatalf("applyEnvOverrides: %v", err)
	}
	if cfg.Metrics.Timeout != 20*time.Second {
		t.Errorf("metrics.timeout = %s, want 20s", cfg.Metrics.Timeout)
	}
	if cfg.Grafana.AlertCheckInterval != time.Minute {
		t.Errorf("grafana.alert_check_interval = %s, want 1m0s", cfg.Grafana.AlertCheckInterval)
	}
	if cfg.Timeline.HistoryWindow != 2*time.Hour {
		t.Errorf("timeline.history_window = %s, want 2h0m0s", cfg.Timeline.HistoryWindow)
	}
	if cfg.Timeline.PollInterval != 45*time.Second {
		t.Errorf("timeline.poll_interval = %s, want 45s", cfg.Timeline.PollInterval)
	}
}

// An unset variable leaves whatever the YAML and defaults produced, so an empty
// environment must not zero a configured duration.
func TestApplyEnvOverridesLeavesUnsetDurationsAlone(t *testing.T) {
	clearDurationVars(t)

	cfg := Config{}
	cfg.Metrics.Timeout = 5 * time.Second
	cfg.Grafana.AlertCheckInterval = 2 * time.Minute
	cfg.Timeline.HistoryWindow = time.Hour
	cfg.Timeline.PollInterval = 30 * time.Second

	if err := applyEnvOverrides(&cfg); err != nil {
		t.Fatalf("applyEnvOverrides: %v", err)
	}
	if cfg.Metrics.Timeout != 5*time.Second {
		t.Errorf("metrics.timeout = %s, want it untouched at 5s", cfg.Metrics.Timeout)
	}
	if cfg.Grafana.AlertCheckInterval != 2*time.Minute {
		t.Errorf("grafana.alert_check_interval = %s, want it untouched at 2m0s", cfg.Grafana.AlertCheckInterval)
	}
	if cfg.Timeline.HistoryWindow != time.Hour {
		t.Errorf("timeline.history_window = %s, want it untouched at 1h0m0s", cfg.Timeline.HistoryWindow)
	}
	if cfg.Timeline.PollInterval != 30*time.Second {
		t.Errorf("timeline.poll_interval = %s, want it untouched at 30s", cfg.Timeline.PollInterval)
	}
}

// Load is what production calls, and nothing else in this module covers it. Without
// this the applyEnvOverrides call could be deleted outright and every test would
// still pass, while every PUSHWARD_* override for the bridge silently stopped
// working - a pod that crashloops on "metrics.url is required" in the cluster and
// nowhere else.
func TestLoadAppliesEnvOverrides(t *testing.T) {
	clearDurationVars(t)
	t.Setenv("PUSHWARD_METRICS_URL", "http://victoria:8428")
	t.Setenv("PUSHWARD_URL", "https://api.pushward.app")
	t.Setenv("PUSHWARD_API_KEY", "hlk_test")
	t.Setenv("PUSHWARD_POLL_INTERVAL", "45s")

	// A missing file is tolerated: the bridge is configurable from the environment
	// alone, which is how it runs in a container.
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Metrics.URL != "http://victoria:8428" {
		t.Errorf("metrics.url = %q, want the env value", cfg.Metrics.URL)
	}
	if cfg.Timeline.PollInterval != 45*time.Second {
		t.Errorf("timeline.poll_interval = %s, want 45s", cfg.Timeline.PollInterval)
	}
}

// TestLoadPushWardDefaults pins the activity tunables, which diverge from every
// other bridge in three ways worth stating out loud.
func TestLoadPushWardDefaults(t *testing.T) {
	clearDurationVars(t)
	t.Setenv("PUSHWARD_METRICS_URL", "http://victoria:8428")
	t.Setenv("PUSHWARD_URL", "https://api.pushward.app")
	t.Setenv("PUSHWARD_API_KEY", "hlk_test")

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Five, not one: an alert outranks a build or a download on the Lock Screen.
	if cfg.PushWard.Priority != 5 {
		t.Errorf("pushward.priority = %d, want 5", cfg.PushWard.Priority)
	}
	if cfg.PushWard.CleanupDelay != 15*time.Minute {
		t.Errorf("pushward.cleanup_delay = %s, want 15m", cfg.PushWard.CleanupDelay)
	}
	// A day, not half an hour: an alert stays firing until something clears it,
	// and a 30m TTL would evict the card while the problem is still live.
	if cfg.PushWard.StaleTimeout != 24*time.Hour {
		t.Errorf("pushward.stale_timeout = %s, want 24h", cfg.PushWard.StaleTimeout)
	}
	// Deliberately zero, and this is the invariant: the bridge ends an activity
	// in one shot rather than the two-phase ONGOING-then-ENDED the pollers use,
	// so nothing here reads these two fields. Giving them the shared 5s/4s would
	// put live-looking values in a struct no code path consults.
	if cfg.PushWard.EndDelay != 0 || cfg.PushWard.EndDisplayTime != 0 {
		t.Errorf("end_delay/end_display_time = %s / %s, want both unset",
			cfg.PushWard.EndDelay, cfg.PushWard.EndDisplayTime)
	}
}

// A typo in a manifest must not quietly leave the default in place.
func TestApplyEnvOverridesRejectsUnparseableDurations(t *testing.T) {
	for _, name := range []string{
		"PUSHWARD_METRICS_TIMEOUT",
		"PUSHWARD_ALERT_CHECK_INTERVAL",
		"PUSHWARD_HISTORY_WINDOW",
		"PUSHWARD_POLL_INTERVAL",
	} {
		t.Run(name, func(t *testing.T) {
			clearDurationVars(t)
			t.Setenv(name, "30")

			var cfg Config
			err := applyEnvOverrides(&cfg)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error should name the variable, got %v", err)
			}
		})
	}
}
