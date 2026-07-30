package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig writes a minimal valid config and returns its path. Tests use
// t.Setenv, so none of them can run in parallel.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimal = `
forgejo:
  url: "https://git.example.com"
  token: "tok"
  owner: "acme"
pushward:
  url: "https://api.example.com"
  api_key: "hlk_test"
`

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Polling.IdleInterval != 60*time.Second {
		t.Errorf("idle interval = %v, want 60s", cfg.Polling.IdleInterval)
	}
	if cfg.Polling.Interval != 15*time.Second {
		t.Errorf("active interval = %v, want 15s", cfg.Polling.Interval)
	}
	if cfg.Forgejo.Timeout != 15*time.Second {
		t.Errorf("timeout = %v, want 15s", cfg.Forgejo.Timeout)
	}
	if cfg.PushWard.Priority != 1 {
		t.Errorf("priority = %d, want 1", cfg.PushWard.Priority)
	}
	if cfg.PushWard.CleanupDelay != 15*time.Minute || cfg.PushWard.StaleTimeout != 30*time.Minute {
		t.Errorf("TTL defaults = %v / %v", cfg.PushWard.CleanupDelay, cfg.PushWard.StaleTimeout)
	}
	// The pill fields are opt-in; the ETA is on, matching the github bridge.
	if cfg.Render.StepColors || cfg.Render.StepWeights {
		t.Error("step colors and weights must default off")
	}
	if !cfg.Render.LiveProgress {
		t.Error("live progress must default on")
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("PUSHWARD_FORGEJO_URL", "https://other.example.com")
	t.Setenv("PUSHWARD_FORGEJO_TOKEN", "env-token")
	t.Setenv("PUSHWARD_FORGEJO_OWNER", "env-owner")
	t.Setenv("PUSHWARD_FORGEJO_REPOS", "a/b, c/d ,,e/f,")
	t.Setenv("PUSHWARD_FORGEJO_TIMEOUT", "30s")
	t.Setenv("PUSHWARD_POLL_IDLE", "10s")
	t.Setenv("PUSHWARD_FORGEJO_STEP_COLORS", "true")
	t.Setenv("PUSHWARD_FORGEJO_STEP_WEIGHTS", "1")
	t.Setenv("PUSHWARD_FORGEJO_LIVE_PROGRESS", "false")

	cfg, err := Load(writeConfig(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Forgejo.URL != "https://other.example.com" || cfg.Forgejo.Token != "env-token" {
		t.Errorf("url/token = %q / %q", cfg.Forgejo.URL, cfg.Forgejo.Token)
	}
	if cfg.Forgejo.Owner != "env-owner" {
		t.Errorf("owner = %q", cfg.Forgejo.Owner)
	}
	// Blank entries and stray whitespace are dropped: an empty repo would fail
	// every poll for the life of the process.
	want := []string{"a/b", "c/d", "e/f"}
	if len(cfg.Forgejo.Repos) != len(want) {
		t.Fatalf("repos = %v, want %v", cfg.Forgejo.Repos, want)
	}
	for i, r := range want {
		if cfg.Forgejo.Repos[i] != r {
			t.Errorf("repos[%d] = %q, want %q", i, cfg.Forgejo.Repos[i], r)
		}
	}
	if cfg.Forgejo.Timeout != 30*time.Second || cfg.Polling.IdleInterval != 10*time.Second {
		t.Errorf("durations = %v / %v", cfg.Forgejo.Timeout, cfg.Polling.IdleInterval)
	}
	// The active tier is resolved after this override, so it follows the lowered idle
	// interval down instead of staying at 15s and outlasting it.
	if cfg.Polling.Interval != 10*time.Second {
		t.Errorf("active interval = %v, want it to follow the 10s idle interval", cfg.Polling.Interval)
	}
	if !cfg.Render.StepColors || !cfg.Render.StepWeights || cfg.Render.LiveProgress {
		t.Errorf("render flags = %+v", cfg.Render)
	}
}

// TestLoadRejectsUnparseableEnv covers the deliberate choice to fail loudly: a
// typo in a manifest must not quietly disable a feature.
func TestLoadRejectsUnparseableEnv(t *testing.T) {
	tests := map[string]string{
		"PUSHWARD_FORGEJO_STEP_COLORS":   "yes-please",
		"PUSHWARD_FORGEJO_STEP_WEIGHTS":  "maybe",
		"PUSHWARD_FORGEJO_LIVE_PROGRESS": "on-ish",
		"PUSHWARD_FORGEJO_TIMEOUT":       "quickly",
		"PUSHWARD_POLL_IDLE":             "sometimes",
	}
	for name, bad := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, bad)
			if _, err := Load(writeConfig(t, minimal)); err == nil {
				t.Errorf("expected an error for %s=%q", name, bad)
			}
		})
	}
}

func TestLoadRequiredFields(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing url",
			body: "forgejo:\n  token: t\n  owner: acme\npushward:\n  url: https://a\n  api_key: hlk_x\n",
			want: "forgejo.url",
		},
		{
			name: "missing token",
			body: "forgejo:\n  url: https://git.example.com\n  owner: acme\npushward:\n  url: https://a\n  api_key: hlk_x\n",
			want: "forgejo.token",
		},
		{
			name: "no owner and no repos",
			body: "forgejo:\n  url: https://git.example.com\n  token: t\npushward:\n  url: https://a\n  api_key: hlk_x\n",
			want: "forgejo.repos or forgejo.owner",
		},
		{
			name: "url without a scheme",
			body: "forgejo:\n  url: git.example.com\n  token: t\n  owner: acme\npushward:\n  url: https://a\n  api_key: hlk_x\n",
			want: "http or https",
		},
		{
			name: "url without a host",
			body: "forgejo:\n  url: \"https://\"\n  token: t\n  owner: acme\npushward:\n  url: https://a\n  api_key: hlk_x\n",
			want: "host",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadRejectsNonPositiveIntervals(t *testing.T) {
	t.Setenv("PUSHWARD_POLL_IDLE", "0s")
	_, err := Load(writeConfig(t, minimal))
	if err == nil {
		t.Fatal("expected an error for a zero poll interval")
	}
	// Names the tier that is wrong, so the operator knows which key to edit.
	if !strings.Contains(err.Error(), "polling.idle_interval") {
		t.Errorf("error should name the field, got %v", err)
	}
}

// TestLoadToleratesMissingFile covers the env-only deployment: the container
// runs with -config /dev/null and every value arrives through the environment.
func TestLoadToleratesMissingFile(t *testing.T) {
	t.Setenv("PUSHWARD_FORGEJO_URL", "https://git.example.com")
	t.Setenv("PUSHWARD_FORGEJO_TOKEN", "tok")
	t.Setenv("PUSHWARD_FORGEJO_OWNER", "acme")
	t.Setenv("PUSHWARD_URL", "https://api.example.com")
	t.Setenv("PUSHWARD_API_KEY", "hlk_test")

	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yml"))
	if err != nil {
		t.Fatalf("a missing config file must be tolerated: %v", err)
	}
	if cfg.Forgejo.Owner != "acme" {
		t.Errorf("owner = %q", cfg.Forgejo.Owner)
	}
}
