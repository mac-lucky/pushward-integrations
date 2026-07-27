package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// TestLoad_LiveProgressDefault pins the one render flag that ships on. The two
// pill flags stay off, so an existing deployment sees the same pills it always
// did and gains only the self-filling step.
func TestLoad_LiveProgressDefault(t *testing.T) {
	t.Setenv("PUSHWARD_GITHUB_LIVE_PROGRESS", "")
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
		{name: "env turns it off", env: "false"},
		{name: "env wins over yaml", yaml: "render:\n  live_progress: false\n", env: "true", want: true},
		// t.Setenv cannot unset, and os.Getenv cannot tell empty from absent, so
		// this is the same path a missing variable takes.
		{name: "empty env leaves the default", env: "", want: true},
		{name: "unparseable env is an error", env: "maybe", fails: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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
