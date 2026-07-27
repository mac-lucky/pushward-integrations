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
	// Both render flags default on: the anchors are additive and the log view
	// is the only readable rendering prune and check have.
	if !cfg.Render.LiveProgress {
		t.Error("render.live_progress defaulted off, want on")
	}
	if !cfg.Render.Logs {
		t.Error("render.logs defaulted off, want on")
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
	// A flag the environment does not mention keeps the file's value.
	if cfg.Backrest.Username != "fileuser" {
		t.Errorf("username = %q, want the file value preserved", cfg.Backrest.Username)
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
	withEnv(t, map[string]string{"PUSHWARD_API_KEY": "hlk_test"})
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
}
