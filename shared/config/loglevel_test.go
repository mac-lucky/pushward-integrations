package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		in    string
		want  slog.Level
		wantK bool
	}{
		{in: "debug", want: slog.LevelDebug, wantK: true},
		// Case and stray whitespace survive a YAML block scalar and a copied
		// kubectl edit, so neither should cost someone their log level.
		{in: "  DEBUG ", want: slog.LevelDebug, wantK: true},
		{in: "info", want: slog.LevelInfo, wantK: true},
		{in: "warn", want: slog.LevelWarn, wantK: true},
		{in: "warning", want: slog.LevelWarn, wantK: true},
		{in: "error", want: slog.LevelError, wantK: true},
		// Unrecognised and unset both report the info fallback, and both say so
		// through ok, which is what lets NewLogger warn about only the first.
		{in: "verbose", want: slog.LevelInfo},
		{in: "", want: slog.LevelInfo},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ParseLogLevel(tc.in)
			if got != tc.want || ok != tc.wantK {
				t.Errorf("ParseLogLevel(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.wantK)
			}
		})
	}
}

// TestNewLogger_Level is the point of the knob: debug has to actually reach the
// handler, because the line naming why a live-progress window was not anchored
// is logged at debug and is otherwise unreachable in a deployed bridge.
func TestNewLogger_Level(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		wantDebug bool
	}{
		{name: "unset defaults to info", wantDebug: false},
		{name: "debug lets debug through", env: "debug", wantDebug: true},
		{name: "an unusable value falls back to info", env: "verbose", wantDebug: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(LogLevelEnv, tc.env)
			if got := NewLogger().Enabled(t.Context(), slog.LevelDebug); got != tc.wantDebug {
				t.Errorf("debug enabled = %v, want %v", got, tc.wantDebug)
			}
		})
	}
}

// A bad value must not be swallowed: the fallback is only acceptable because the
// operator is told about it, and the very first line of the log is where someone
// who just edited the variable will look.
func TestNewLogger_WarnsOnAnUnusableValue(t *testing.T) {
	t.Setenv(LogLevelEnv, "verbose")

	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Both ends close even when an assertion below fails the test early, so a
	// -count=N run cannot leak a descriptor per iteration. Swapping the global is
	// safe here only because t.Setenv already forbids t.Parallel in this package.
	defer func() { _ = r.Close() }()
	os.Stdout = w
	NewLogger()
	os.Stdout = stdout
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := out.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	var line struct {
		Level string `json:"level"`
		Msg   string `json:"msg"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("expected one JSON log line, got %q: %v", out.String(), err)
	}
	if line.Level != "WARN" || line.Value != "verbose" {
		t.Errorf("got level=%q value=%q, want WARN and the rejected value", line.Level, line.Value)
	}
}
