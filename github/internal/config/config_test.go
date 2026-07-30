package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearPollEnv makes a test independent of the shell it runs in. Both names are
// shared across six bridges, so an exported pair can be mutually invalid and fail
// Load for a reason the test is not about. Call it before a test sets its own values.
func clearPollEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PUSHWARD_POLL_IDLE", "")
	t.Setenv("PUSHWARD_POLL_INTERVAL", "")
}

// writeConfig writes a config file carrying the required credentials plus the
// given render block, and returns its path.
func writeConfig(t *testing.T, render string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	body := "github:\n  token: \"ghp_test\"\n  owner: \"owner\"\npushward:\n  url: \"https://example.test\"\n  api_key: \"hlk_test\"\n" + render
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoad_PushWardDefaults pins the activity tunables this bridge ships. They
// are the plain shared set, with no per-bridge divergence.
func TestLoad_PushWardDefaults(t *testing.T) {
	clearPollEnv(t)
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PushWard.Priority != 1 {
		t.Errorf("priority = %d, want 1", cfg.PushWard.Priority)
	}
	if cfg.PushWard.CleanupDelay != 15*time.Minute {
		t.Errorf("cleanup_delay = %v, want 15m", cfg.PushWard.CleanupDelay)
	}
	if cfg.PushWard.StaleTimeout != 30*time.Minute {
		t.Errorf("stale_timeout = %v, want 30m", cfg.PushWard.StaleTimeout)
	}
	if cfg.PushWard.EndDelay != 5*time.Second {
		t.Errorf("end_delay = %v, want 5s", cfg.PushWard.EndDelay)
	}
	if cfg.PushWard.EndDisplayTime != 4*time.Second {
		t.Errorf("end_display_time = %v, want 4s", cfg.PushWard.EndDisplayTime)
	}
}

// TestLoad_LiveProgressDefault pins the one render flag that ships on. The two
// pill flags stay off, so an existing deployment sees the same pills it always
// did and gains only the self-filling step.
func TestLoad_LiveProgressDefault(t *testing.T) {
	clearPollEnv(t)
	// Clear all three: an exported pill flag in the developer's shell would
	// otherwise fail this for an unrelated reason.
	t.Setenv("PUSHWARD_GITHUB_LIVE_PROGRESS", "")
	t.Setenv("PUSHWARD_GITHUB_STEP_COLORS", "")
	t.Setenv("PUSHWARD_GITHUB_STEP_WEIGHTS", "")
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Render.LiveProgress {
		t.Error("live_progress must default on")
	}
	if cfg.Render.StepColors || cfg.Render.StepWeights {
		t.Errorf("pill flags must stay opt-in, got colors=%v weights=%v",
			cfg.Render.StepColors, cfg.Render.StepWeights)
	}
}

// Subtests here deliberately do not call t.Parallel: t.Setenv panics in a test
// with a parallel ancestor.
func TestLoad_LiveProgressOverrides(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		env   string
		want  bool
		fails bool
	}{
		{name: "yaml turns it off", yaml: "render:\n  live_progress: false\n"},
		{name: "yaml restates the default", yaml: "render:\n  live_progress: true\n", want: true},
		// The upgrade path: a deployment that already configures the pill flags
		// must keep the new default rather than have it decoded away.
		{name: "render block without the key keeps the default", yaml: "render:\n  step_colors: true\n", want: true},
		{name: "env turns it off", env: "false"},
		{name: "env wins over yaml", yaml: "render:\n  live_progress: false\n", env: "true", want: true},
		// t.Setenv cannot unset, and os.Getenv cannot tell empty from absent, so
		// this is the same path a missing variable takes.
		{name: "empty env leaves the default", env: "", want: true},
		{name: "unparseable env is an error", env: "maybe", fails: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearPollEnv(t)
			t.Setenv("PUSHWARD_GITHUB_LIVE_PROGRESS", tc.env)
			cfg, err := Load(writeConfig(t, tc.yaml))
			if tc.fails {
				if err == nil {
					t.Fatal("expected an error for an unparseable flag")
				}
				if !strings.Contains(err.Error(), "PUSHWARD_GITHUB_LIVE_PROGRESS") {
					t.Errorf("error must name the variable, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Render.LiveProgress != tc.want {
				t.Errorf("live_progress = %v, want %v", cfg.Render.LiveProgress, tc.want)
			}
		})
	}
}

// A non-positive poll interval used to reach time.NewTicker and panic inside the
// shared poller, after the bridge had already created an activity per repo. The
// forgejo bridge always rejected it; this one now does too.
func TestLoad_RejectsNonPositivePollInterval(t *testing.T) {
	for _, v := range []string{"0s", "-1s"} {
		t.Run(v, func(t *testing.T) {
			clearPollEnv(t)
			t.Setenv("PUSHWARD_POLL_IDLE", v)
			_, err := Load(writeConfig(t, ""))
			if err == nil {
				t.Fatalf("expected %s to be rejected", v)
			}
			if !strings.Contains(err.Error(), "idle_interval") {
				t.Errorf("error should name the field, got %v", err)
			}
		})
	}
}

// The rule itself is shared/config's; what this pins is the wiring - Load applies
// it AFTER the env overrides. An operator who lowered idle_interval to get faster
// cards must not end up with an active tier slower than the idle one, which is
// exactly what resolving too early would produce.
func TestLoad_ResolvesTheActiveIntervalAfterTheEnvOverrides(t *testing.T) {
	tests := []struct {
		name     string
		idle     string
		active   string
		wantIdle time.Duration
		want     time.Duration
	}{
		{name: "unset takes 15s under the 60s default", wantIdle: 60 * time.Second, want: 15 * time.Second},
		{name: "follows an idle interval below 15s", idle: "10s", wantIdle: 10 * time.Second, want: 10 * time.Second},
		// The derived default must not clobber a value the operator set.
		{name: "an explicit value wins", idle: "60s", active: "5s", wantIdle: 60 * time.Second, want: 5 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PUSHWARD_POLL_IDLE", tc.idle)
			t.Setenv("PUSHWARD_POLL_INTERVAL", tc.active)
			cfg, err := Load(writeConfig(t, ""))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Polling.Interval != tc.want {
				t.Errorf("polling.interval = %s, want %s", cfg.Polling.Interval, tc.want)
			}
			if cfg.Polling.IdleInterval != tc.wantIdle {
				t.Errorf("polling.idle_interval = %s, want %s", cfg.Polling.IdleInterval, tc.wantIdle)
			}
		})
	}
}

func TestLoad_RejectsNonPositiveActiveInterval(t *testing.T) {
	clearPollEnv(t)
	t.Setenv("PUSHWARD_POLL_INTERVAL", "-1s")
	_, err := Load(writeConfig(t, ""))
	if err == nil {
		t.Fatal("expected a negative active interval to be rejected")
	}
	// Not a bare "interval": that is a substring of "idle_interval" too, so it would
	// pass on an error naming the wrong tier.
	if !strings.Contains(err.Error(), "polling.interval") {
		t.Errorf("error should name the active tier, got %v", err)
	}
	if strings.Contains(err.Error(), "polling.idle_interval") {
		t.Errorf("the idle tier was reported for an active-tier problem: %v", err)
	}
}

// An active tier slower than the idle one is nonsense: the loop ticks on the active
// interval and gates detection off the idle one, so it would silently stretch
// detection instead. The rule lives in shared/config; what this pins is that Load
// still routes through it rather than resolving the conflict itself.
func TestLoad_RejectsAnActiveIntervalSlowerThanIdle(t *testing.T) {
	t.Setenv("PUSHWARD_POLL_IDLE", "30s")
	t.Setenv("PUSHWARD_POLL_INTERVAL", "60s")
	_, err := Load(writeConfig(t, ""))
	if err == nil {
		t.Fatal("expected an active interval above the idle one to be rejected")
	}
	if !strings.Contains(err.Error(), "must not exceed") {
		t.Errorf("error should name the cross-field rule, got %v", err)
	}
}

// A trailing comma in the repo list used to produce an empty repo name that
// failed every poll for the life of the process.
func TestLoad_RepoListToleratesATrailingComma(t *testing.T) {
	clearPollEnv(t)
	t.Setenv("PUSHWARD_GITHUB_REPOS", "owner/one, owner/two,")
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"owner/one", "owner/two"}
	if len(cfg.GitHub.Repos) != len(want) {
		t.Fatalf("repos = %v, want %v", cfg.GitHub.Repos, want)
	}
	for i, r := range want {
		if cfg.GitHub.Repos[i] != r {
			t.Errorf("repos[%d] = %q, want %q", i, cfg.GitHub.Repos[i], r)
		}
	}
}
