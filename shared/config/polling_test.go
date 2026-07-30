package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultPollingConfig(t *testing.T) {
	p := DefaultPollingConfig()
	if p.IdleInterval != 60*time.Second {
		t.Errorf("IdleInterval = %s, want 1m0s", p.IdleInterval)
	}
	// Interval must stay unset here so ApplyActiveDefault can derive it from
	// whatever IdleInterval survives the override layers. Pre-filling 15s would
	// make an idle_interval below that a cross-field error.
	if p.Interval != 0 {
		t.Errorf("Interval = %s, want it left unset", p.Interval)
	}
}

func TestPollingConfigApplyEnvOverrides(t *testing.T) {
	t.Setenv("PUSHWARD_POLL_IDLE", "30s")
	t.Setenv("PUSHWARD_POLL_INTERVAL", "5s")

	p := DefaultPollingConfig()
	if err := p.ApplyEnvOverrides(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.IdleInterval != 30*time.Second {
		t.Errorf("IdleInterval = %s, want 30s", p.IdleInterval)
	}
	if p.Interval != 5*time.Second {
		t.Errorf("Interval = %s, want 5s", p.Interval)
	}
}

// The two variables are deliberately unprefixed, unlike RenderConfig's: every
// manifest in production sets these exact names for whichever bridge is in the
// container.
func TestPollingConfigApplyEnvOverridesTakesNoBridgePrefix(t *testing.T) {
	// Cleared rather than assumed absent: an inherited value in the developer's shell
	// would otherwise make this pass for the wrong reason.
	t.Setenv("PUSHWARD_POLL_IDLE", "")
	t.Setenv("PUSHWARD_POLL_INTERVAL", "")
	t.Setenv("PUSHWARD_GITHUB_POLL_IDLE", "5m")

	p := DefaultPollingConfig()
	if err := p.ApplyEnvOverrides(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.IdleInterval != 60*time.Second {
		t.Errorf("a prefixed variable was read: IdleInterval = %s, want the 1m0s default", p.IdleInterval)
	}
}

func TestPollingConfigApplyEnvOverridesRejectsGarbage(t *testing.T) {
	for _, name := range []string{"PUSHWARD_POLL_IDLE", "PUSHWARD_POLL_INTERVAL"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "30")

			p := DefaultPollingConfig()
			err := p.ApplyEnvOverrides()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error should name the variable, got %v", err)
			}
		})
	}
}

func TestPollingConfigApplyActiveDefault(t *testing.T) {
	tests := []struct {
		name string
		in   PollingConfig
		want time.Duration
	}{
		{
			name: "unset takes 15s under the 60s default",
			in:   PollingConfig{IdleInterval: 60 * time.Second},
			want: 15 * time.Second,
		},
		{
			name: "follows an idle interval below 15s down",
			in:   PollingConfig{IdleInterval: 10 * time.Second},
			want: 10 * time.Second,
		},
		{
			name: "an explicit value wins",
			in:   PollingConfig{IdleInterval: 60 * time.Second, Interval: 5 * time.Second},
			want: 5 * time.Second,
		},
		{
			name: "a long idle interval still caps at 15s",
			in:   PollingConfig{IdleInterval: 5 * time.Minute},
			want: 15 * time.Second,
		},
		{
			// A mistake, not an unset value: it must reach Validate rather than be
			// quietly turned into something workable.
			name: "a negative value is left alone",
			in:   PollingConfig{IdleInterval: 60 * time.Second, Interval: -time.Second},
			want: -time.Second,
		},
		{
			// Also not an unset value. Repairing this here instead would make
			// Validate's cross-field rejection unreachable on every path that
			// resolves before validating - which is all of them.
			name: "a value slower than the idle tier is left alone",
			in:   PollingConfig{IdleInterval: 30 * time.Second, Interval: 60 * time.Second},
			want: 60 * time.Second,
		},
		{
			// The one input where the derived value is itself zero, so the second
			// application re-enters the branch. It is a fixed point; Validate is what
			// reports the real problem, the idle tier.
			name: "a zero idle interval leaves the active tier for Validate",
			in:   PollingConfig{},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.in
			p.ApplyActiveDefault()
			if p.Interval != tt.want {
				t.Errorf("Interval = %s, want %s", p.Interval, tt.want)
			}
			if p.IdleInterval != tt.in.IdleInterval {
				t.Errorf("IdleInterval changed to %s, want %s", p.IdleInterval, tt.in.IdleInterval)
			}

			// Idempotent, so a caller that resolves twice - or a bridge that hands an
			// already-resolved config to cipoll, which resolves again - gets the same
			// cadence rather than a second round of inheritance.
			p.ApplyActiveDefault()
			if p.Interval != tt.want {
				t.Errorf("second call changed Interval to %s, want %s", p.Interval, tt.want)
			}
		})
	}
}

func TestPollingConfigValidate(t *testing.T) {
	tests := []struct {
		name string
		p    PollingConfig
		want string // empty means it must pass
	}{
		{
			name: "the shipped default resolved",
			p:    PollingConfig{IdleInterval: 60 * time.Second, Interval: 15 * time.Second},
		},
		{
			name: "equal tiers are legal",
			p:    PollingConfig{IdleInterval: 10 * time.Second, Interval: 10 * time.Second},
		},
		{
			name: "zero idle",
			p:    PollingConfig{Interval: 5 * time.Second},
			want: "polling.idle_interval",
		},
		{
			name: "negative idle",
			p:    PollingConfig{IdleInterval: -time.Second, Interval: 5 * time.Second},
			want: "polling.idle_interval",
		},
		{
			name: "zero active",
			p:    PollingConfig{IdleInterval: 60 * time.Second},
			want: "polling.interval",
		},
		{
			name: "negative active",
			p:    PollingConfig{IdleInterval: 60 * time.Second, Interval: -time.Second},
			want: "polling.interval",
		},
		{
			name: "an active tier slower than the idle one",
			p:    PollingConfig{IdleInterval: 30 * time.Second, Interval: 60 * time.Second},
			want: "must not exceed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.p.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should mention %q, got %v", tt.want, err)
			}
			// "polling.interval" is not a substring of "polling.idle_interval", so a
			// message naming the wrong tier fails here rather than passing on a
			// coincidental match.
			if tt.want == "polling.interval" && strings.Contains(err.Error(), "polling.idle_interval") {
				t.Errorf("the idle tier was reported for an active-tier problem: %v", err)
			}
		})
	}
}
