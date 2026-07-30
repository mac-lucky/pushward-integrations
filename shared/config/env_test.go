package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSplitList(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "plain", in: "a/b,c/d", want: []string{"a/b", "c/d"}},
		// The bug this exists for: a trailing comma used to yield an empty entry,
		// and an empty repo name fails every poll for the life of the process.
		{name: "trailing comma", in: "a/b,", want: []string{"a/b"}},
		{name: "surrounding whitespace", in: " a/b , c/d ", want: []string{"a/b", "c/d"}},
		{name: "wrapped yaml value", in: "a/b,\n  c/d", want: []string{"a/b", "c/d"}},
		{name: "only separators", in: ",,", want: []string{}},
		{name: "empty", in: "", want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitList(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("SplitList(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// The zero value is not the shipped default, so a bridge starting from
// RenderConfig{} would ship live progress off while the field's doc says it
// defaults on.
func TestDefaultRenderConfig(t *testing.T) {
	d := DefaultRenderConfig()
	if !d.LiveProgress {
		t.Error("live progress must default on")
	}
	if d.StepColors || d.StepWeights {
		t.Errorf("the pill fields must default off, got %+v", d)
	}
	if (RenderConfig{}) == d {
		t.Error("if the zero value ever equals the default, this helper is pointless")
	}
}

func TestEnvBool(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		initial bool
		want    bool
		wantErr bool
	}{
		{name: "unset leaves the default", value: "", initial: true, want: true},
		{name: "true turns it on", value: "true", initial: false, want: true},
		{name: "false turns it off", value: "false", initial: true, want: false},
		{name: "numeric form", value: "1", initial: false, want: true},
		// A flag that defaults on must not stay on because someone wrote a word
		// ParseBool does not accept - the startup fails instead.
		{name: "unparseable is an error", value: "yes", initial: true, want: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != "" {
				t.Setenv("PUSHWARD_TEST_FLAG", tt.value)
			}
			got := tt.initial
			err := EnvBool("PUSHWARD_TEST_FLAG", &got)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !strings.Contains(err.Error(), "PUSHWARD_TEST_FLAG") {
					t.Errorf("error should name the variable, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnvInt(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    int
		wantErr bool
	}{
		{name: "unset leaves the default", value: "", want: 1},
		{name: "parsed", value: "7", want: 7},
		{name: "negative", value: "-3", want: -3},
		// The reason this is Atoi and not Sscanf: both of these would otherwise
		// parse to a number the operator never wrote.
		{name: "trailing garbage is an error", value: "5x", want: 1, wantErr: true},
		{name: "hex is an error", value: "0x10", want: 1, wantErr: true},
		{name: "a float is an error", value: "1.5", want: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != "" {
				t.Setenv("PUSHWARD_TEST_COUNT", tt.value)
			}
			got := 1
			err := EnvInt("PUSHWARD_TEST_COUNT", &got)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !strings.Contains(err.Error(), "PUSHWARD_TEST_COUNT") {
					t.Errorf("error should name the variable, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnvInt64(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    int64
		wantErr bool
	}{
		{name: "unset leaves the default", value: "", want: 10},
		{name: "parsed", value: "25", want: 25},
		// The width is the point: this must survive a value an int32 cannot hold.
		{name: "beyond 32 bits", value: "4294967296", want: 4294967296},
		{name: "trailing garbage is an error", value: "25n", want: 10, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != "" {
				t.Setenv("PUSHWARD_TEST_LAST_N", tt.value)
			}
			got := int64(10)
			err := EnvInt64("PUSHWARD_TEST_LAST_N", &got)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !strings.Contains(err.Error(), "PUSHWARD_TEST_LAST_N") {
					t.Errorf("error should name the variable, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnvFloat64(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    float64
		wantErr bool
	}{
		{name: "unset leaves the default", value: "", want: 1.0},
		{name: "parsed", value: "0.25", want: 0.25},
		{name: "an integer form parses", value: "0", want: 0},
		// Out of range is the caller's call, not the parser's: this one is clamped
		// downstream rather than rejected here.
		{name: "out of range still parses", value: "2.5", want: 2.5},
		{name: "unparseable is an error", value: "half", want: 1.0, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != "" {
				t.Setenv("PUSHWARD_TEST_SAMPLE_RATE", tt.value)
			}
			got := 1.0
			err := EnvFloat64("PUSHWARD_TEST_SAMPLE_RATE", &got)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !strings.Contains(err.Error(), "PUSHWARD_TEST_SAMPLE_RATE") {
					t.Errorf("error should name the variable, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnvDuration(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset leaves the default", value: "", want: 30 * time.Second},
		{name: "parsed", value: "90s", want: 90 * time.Second},
		{name: "unparseable is an error", value: "90", want: 30 * time.Second, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != "" {
				t.Setenv("PUSHWARD_TEST_INTERVAL", tt.value)
			}
			got := 30 * time.Second
			err := EnvDuration("PUSHWARD_TEST_INTERVAL", &got)
			if tt.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Callers rely on the message identifying which variable was wrong: the
			// bridges pass several of these in a row and report the first failure.
			if tt.wantErr && !strings.Contains(err.Error(), "PUSHWARD_TEST_INTERVAL") {
				t.Errorf("error should name the variable, got %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
