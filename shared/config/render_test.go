package config

import (
	"strings"
	"testing"
)

func TestRenderConfigApplyEnvOverrides(t *testing.T) {
	t.Setenv("PUSHWARD_GITHUB_STEP_COLORS", "true")
	t.Setenv("PUSHWARD_GITHUB_STEP_WEIGHTS", "true")
	t.Setenv("PUSHWARD_GITHUB_LIVE_PROGRESS", "false")

	// A different bridge's variables must not leak across.
	t.Setenv("PUSHWARD_FORGEJO_STEP_COLORS", "false")

	r := RenderConfig{LiveProgress: true}
	if err := r.ApplyEnvOverrides("PUSHWARD_GITHUB"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.StepColors || !r.StepWeights {
		t.Errorf("pill fields should be on, got %+v", r)
	}
	if r.LiveProgress {
		t.Error("live progress should have been switched off")
	}
}

func TestRenderConfigApplyEnvOverridesRejectsGarbage(t *testing.T) {
	t.Setenv("PUSHWARD_FORGEJO_STEP_WEIGHTS", "enabled")
	r := RenderConfig{}
	err := r.ApplyEnvOverrides("PUSHWARD_FORGEJO")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "PUSHWARD_FORGEJO_STEP_WEIGHTS") {
		t.Errorf("error should name the variable, got %v", err)
	}
}

func TestRenderConfigWantTimings(t *testing.T) {
	tests := []struct {
		name string
		r    RenderConfig
		want bool
	}{
		{name: "both off", r: RenderConfig{}, want: false},
		{name: "pill sizing alone", r: RenderConfig{StepWeights: true}, want: true},
		{name: "live progress alone", r: RenderConfig{LiveProgress: true}, want: true},
		{name: "colors alone need no timings", r: RenderConfig{StepColors: true}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.WantTimings(); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
