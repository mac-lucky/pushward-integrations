package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withEnv sets variables for one test and restores the environment after.
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// baseEnv is the minimum that makes Load succeed, so a test can set exactly the
// one variable it is about.
func baseEnv(t *testing.T) {
	t.Helper()
	withEnv(t, map[string]string{
		"PUSHWARD_BACKREST_URL": "http://backrest:9898",
		"PUSHWARD_URL":          "https://api.pushward.app",
		"PUSHWARD_API_KEY":      "hlk_test",
		// Cleared, not assumed absent: both names are shared across six bridges, so an
		// exported value in the developer's shell would fail the default assertions for
		// an unrelated reason. Tests that want a value set it after calling this.
		"PUSHWARD_POLL_INTERVAL": "",
		"PUSHWARD_POLL_IDLE":     "",
	})
}

// Config loading tolerates a missing file: the whole bridge is configurable
// from the environment, which is how it runs in a container.
func TestMissingConfigFileIsFine(t *testing.T) {
	baseEnv(t)
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backrest.URL != "http://backrest:9898" {
		t.Errorf("url = %q", cfg.Backrest.URL)
	}
}

func TestDefaults(t *testing.T) {
	baseEnv(t)
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Polling.Interval != 5*time.Second {
		t.Errorf("polling.interval = %v, want 5s", cfg.Polling.Interval)
	}
	if cfg.Polling.IdleInterval != 30*time.Second {
		t.Errorf("polling.idle_interval = %v, want 30s", cfg.Polling.IdleInterval)
	}
	if cfg.Polling.LastN != 50 {
		t.Errorf("polling.last_n = %d, want 50", cfg.Polling.LastN)
	}
	// cleanup_delay is the activity's ended_ttl. Ten minutes, not the shared
	// fifteen: it is how long a finished backup sits on the Lock Screen.
	if cfg.PushWard.CleanupDelay != 10*time.Minute {
		t.Errorf("pushward.cleanup_delay = %v, want 10m", cfg.PushWard.CleanupDelay)
	}
	// Both render flags default on: the anchors are additive and the log view
	// is the only readable rendering prune and check have.
	if !cfg.Render.LiveProgress {
		t.Error("render.live_progress defaulted off, want on")
	}
	if !cfg.Render.Logs {
		t.Error("render.logs defaulted off, want on")
	}
	// A week, not a working day: a multi-terabyte first run over a slow link is
	// two days out and still deserves its countdown.
	if cfg.Render.MaxETA != 7*24*time.Hour {
		t.Errorf("render.max_eta = %v, want 168h", cfg.Render.MaxETA)
	}
}

func TestURLIsRequired(t *testing.T) {
	withEnv(t, map[string]string{
		"PUSHWARD_URL":     "https://api.pushward.app",
		"PUSHWARD_API_KEY": "hlk_test",
	})
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yml")); err == nil {
		t.Fatal("Load succeeded with no backrest.url")
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	yaml := `
backrest:
  url: http://from-file:9898
  username: fileuser
polling:
  interval: 99s
render:
  live_progress: true
  max_eta: 90h
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	withEnv(t, map[string]string{
		"PUSHWARD_URL":                    "https://api.pushward.app",
		"PUSHWARD_API_KEY":                "hlk_test",
		"PUSHWARD_BACKREST_URL":           "http://from-env:9898",
		"PUSHWARD_POLL_INTERVAL":          "7s",
		"PUSHWARD_BACKREST_LIVE_PROGRESS": "false",
	})

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backrest.URL != "http://from-env:9898" {
		t.Errorf("url = %q, want the environment to win", cfg.Backrest.URL)
	}
	if cfg.Polling.Interval != 7*time.Second {
		t.Errorf("interval = %v, want the environment to win", cfg.Polling.Interval)
	}
	// A setting the environment does not mention keeps the file's value.
	if cfg.Backrest.Username != "fileuser" {
		t.Errorf("username = %q, want the file value preserved", cfg.Backrest.Username)
	}
	if cfg.Render.MaxETA != 90*time.Hour {
		t.Errorf("max_eta = %v, want the file value preserved", cfg.Render.MaxETA)
	}
	if cfg.Render.LiveProgress {
		t.Error("live_progress stayed on, want the environment to turn it off")
	}
}

// A flag that defaults on would otherwise stay on for anyone who wrote "yes"
// and believed they had turned it off.
func TestUnparseableBoolIsAnError(t *testing.T) {
	baseEnv(t)
	t.Setenv("PUSHWARD_BACKREST_LOGS", "yes-please")
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yml")); err == nil {
		t.Fatal("Load accepted an unparseable boolean")
	}
}

func TestUnparseableDurationIsAnError(t *testing.T) {
	baseEnv(t)
	t.Setenv("PUSHWARD_POLL_INTERVAL", "every 5 seconds")
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yml")); err == nil {
		t.Fatal("Load accepted an unparseable duration")
	}
}

// A non-positive interval would spin the poll loop as fast as the CPU allows.
func TestNonPositiveIntervalsAreRejected(t *testing.T) {
	for _, v := range []string{"0s", "-5s"} {
		baseEnv(t)
		t.Setenv("PUSHWARD_POLL_INTERVAL", v)
		if _, err := Load(filepath.Join(t.TempDir(), "absent.yml")); err == nil {
			t.Errorf("Load accepted polling.interval = %s", v)
		}
	}
}

// Zero would reject every estimate, which is not what a ceiling means.
func TestNonPositiveMaxETAIsRejected(t *testing.T) {
	for _, v := range []string{"0s", "-1h"} {
		baseEnv(t)
		t.Setenv("PUSHWARD_BACKREST_MAX_ETA", v)
		if _, err := Load(filepath.Join(t.TempDir(), "absent.yml")); err == nil {
			t.Errorf("Load accepted render.max_eta = %s", v)
		}
	}
}

func TestMaxETAEnvOverride(t *testing.T) {
	baseEnv(t)
	t.Setenv("PUSHWARD_BACKREST_MAX_ETA", "36h")
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Render.MaxETA != 36*time.Hour {
		t.Errorf("render.max_eta = %v, want 36h", cfg.Render.MaxETA)
	}
}

func TestNonPositiveLastNIsRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("PUSHWARD_BACKREST_LAST_N", "0")
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yml")); err == nil {
		t.Fatal("Load accepted polling.last_n = 0, which would query nothing")
	}
}

// Backrest lets every request through when auth is disabled, so no credentials
// has to load cleanly rather than failing validation.
func TestNoCredentialsIsValid(t *testing.T) {
	baseEnv(t)
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backrest.Username != "" || cfg.Backrest.Password != "" || cfg.Backrest.Token != "" {
		t.Error("credentials appeared from nowhere")
	}
}

// The example config is what users copy, so it has to parse and agree with the
// defaults it documents.
func TestExampleConfigLoads(t *testing.T) {
	withEnv(t, map[string]string{
		"PUSHWARD_API_KEY": "hlk_test",
		// The point is what the file documents, so the environment must not speak.
		"PUSHWARD_POLL_INTERVAL": "",
		"PUSHWARD_POLL_IDLE":     "",
	})
	cfg, err := Load(filepath.Join("..", "..", "config.example.yml"))
	if err != nil {
		t.Fatalf("loading config.example.yml: %v", err)
	}
	if cfg.Polling.Interval != 5*time.Second {
		t.Errorf("example polling.interval = %v, want 5s", cfg.Polling.Interval)
	}
	if !cfg.Render.Logs || !cfg.Render.LiveProgress {
		t.Error("example disables a render flag that ships on")
	}
	if cfg.Render.MaxETA != 7*24*time.Hour {
		t.Errorf("example render.max_eta = %v, want the documented 168h default", cfg.Render.MaxETA)
	}
	if cfg.PushWard.CleanupDelay != 10*time.Minute {
		t.Errorf("example pushward.cleanup_delay = %v, want the documented 10m", cfg.PushWard.CleanupDelay)
	}
}
