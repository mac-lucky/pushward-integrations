package config

import (
	"reflect"
	"testing"
	"time"
)

// relayEnvVars is every variable applyEnvOverrides reads. Clearing the whole set
// rather than the one under test keeps a developer shell that exports any of them
// from failing a test for a reason the test is not about.
var relayEnvVars = []string{
	"PUSHWARD_DATABASE_DSN",
	"PUSHWARD_DATABASE_PASSWORD_FILE",
	"PUSHWARD_OTEL_ENDPOINT",
	"PUSHWARD_OTEL_TLS_CERT_PATH",
	"PUSHWARD_OTEL_TLS_KEY_PATH",
	"PUSHWARD_OTEL_SAMPLE_RATE",
	"PUSHWARD_TRUSTED_PROXY_CIDRS",
	"PUSHWARD_GRAFANA_ENABLED",
	"PUSHWARD_ARGOCD_ENABLED",
	"PUSHWARD_STARR_ENABLED",
	"PUSHWARD_STARR_MODE",
	"PUSHWARD_GITEA_ENABLED",
	"PUSHWARD_ARGOCD_URL",
	"PUSHWARD_ARGOCD_SYNC_GRACE_PERIOD",
	"PUSHWARD_SYNC_GRACE_PERIOD",
}

// clearRelayEnv makes a test independent of the shell it runs in. Call it before
// a test sets its own values.
func clearRelayEnv(t *testing.T) {
	t.Helper()
	for _, name := range relayEnvVars {
		t.Setenv(name, "")
	}
}

// validConfig returns a Config whose provider list passes both
// validateProviderTimeouts and validatePriorities: a couple of enabled
// providers with positive timeouts and in-range priorities, the rest left as
// disabled zero values (which are valid — a disabled provider is never checked
// for stale_timeout, and priority 0 is in range).
func validConfig() *Config {
	cfg := &Config{}
	cfg.Providers.Grafana.Enabled = true
	cfg.Providers.Grafana.Priority = 10
	cfg.Providers.Grafana.StaleTimeout = 24 * time.Hour

	cfg.Providers.ArgoCD.Enabled = true
	cfg.Providers.ArgoCD.Priority = 3
	cfg.Providers.ArgoCD.StaleTimeout = 30 * time.Minute
	cfg.Providers.ArgoCD.SyncGracePeriod = 10 * time.Second
	return cfg
}

// Subtests here deliberately do not call t.Parallel: t.Setenv panics in a test
// with a parallel ancestor.
func TestApplyEnvOverrides_TrustedProxyCIDRs(t *testing.T) {
	const fromYAML = "192.168.0.0/16"

	tests := []struct {
		name string
		env  string
		want []string
	}{
		{
			name: "unset leaves the YAML value",
			env:  "",
			want: []string{fromYAML},
		},
		{
			name: "a list becomes one entry per CIDR",
			env:  "10.0.0.0/8,172.16.0.0/12",
			want: []string{"10.0.0.0/8", "172.16.0.0/12"},
		},
		{
			// The regression this test exists for. A trailing comma used to leave
			// an empty entry, which reached net.ParseCIDR("") in
			// ratelimit.SetTrustedProxyCIDRs and exited the process at startup.
			name: "a trailing comma does not leave a blank entry",
			env:  "10.0.0.0/8,",
			want: []string{"10.0.0.0/8"},
		},
		{
			name: "surrounding whitespace is trimmed",
			env:  " 10.0.0.0/8 , 172.16.0.0/12 ",
			want: []string{"10.0.0.0/8", "172.16.0.0/12"},
		},
		{
			// All-blank must come out empty, not a slice of empty strings: main
			// gates on len() to choose between configuring the proxies and warning
			// that none are set, and a non-empty slice of blanks fails the parse.
			name: "an all-blank value comes out empty",
			env:  ",, ,",
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearRelayEnv(t)
			t.Setenv("PUSHWARD_TRUSTED_PROXY_CIDRS", tt.env)

			cfg := &Config{TrustedProxyCIDRs: []string{fromYAML}}
			if err := cfg.applyEnvOverrides(); err != nil {
				t.Fatalf("applyEnvOverrides() error = %v", err)
			}
			if !reflect.DeepEqual(cfg.TrustedProxyCIDRs, tt.want) {
				t.Errorf("TrustedProxyCIDRs = %#v, want %#v", cfg.TrustedProxyCIDRs, tt.want)
			}
		})
	}
}

func TestValidateProviderTimeouts(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{
			name:    "valid config passes",
			mutate:  func(*Config) {},
			wantErr: false,
		},
		{
			name: "enabled provider with zero stale timeout errors",
			mutate: func(c *Config) {
				c.Providers.Grafana.Enabled = true
				c.Providers.Grafana.StaleTimeout = 0
			},
			wantErr: true,
		},
		{
			name: "enabled provider with negative stale timeout errors",
			mutate: func(c *Config) {
				c.Providers.Grafana.Enabled = true
				c.Providers.Grafana.StaleTimeout = -time.Second
			},
			wantErr: true,
		},
		{
			name: "disabled provider with zero stale timeout is ok",
			mutate: func(c *Config) {
				c.Providers.Grafana.Enabled = false
				c.Providers.Grafana.StaleTimeout = 0
			},
			wantErr: false,
		},
		{
			// Pins that the shared baseProviders() list covers later-added
			// providers (not just the first few), so a zero TTL on any of them
			// is caught.
			name: "enabled backrest with zero stale timeout errors",
			mutate: func(c *Config) {
				c.Providers.Backrest.Enabled = true
				c.Providers.Backrest.Priority = 2
				c.Providers.Backrest.StaleTimeout = 0
			},
			wantErr: true,
		},
		{
			name: "argocd enabled with negative sync grace period errors",
			mutate: func(c *Config) {
				c.Providers.ArgoCD.Enabled = true
				c.Providers.ArgoCD.StaleTimeout = 30 * time.Minute
				c.Providers.ArgoCD.SyncGracePeriod = -time.Second
			},
			wantErr: true,
		},
		{
			name: "argocd enabled with zero sync grace period is ok",
			mutate: func(c *Config) {
				c.Providers.ArgoCD.Enabled = true
				c.Providers.ArgoCD.StaleTimeout = 30 * time.Minute
				c.Providers.ArgoCD.SyncGracePeriod = 0
			},
			wantErr: false,
		},
		{
			// A disabled argocd with a negative grace period must not error —
			// the guard only applies when argocd is enabled.
			name: "argocd disabled with negative sync grace period is ok",
			mutate: func(c *Config) {
				c.Providers.ArgoCD.Enabled = false
				c.Providers.ArgoCD.StaleTimeout = 0
				c.Providers.ArgoCD.SyncGracePeriod = -time.Hour
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(cfg)
			err := cfg.validateProviderTimeouts()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateProviderTimeouts() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidatePriorities(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{
			name:    "valid config passes",
			mutate:  func(*Config) {},
			wantErr: false,
		},
		{
			name:    "priority 0 is ok",
			mutate:  func(c *Config) { c.Providers.Grafana.Priority = 0 },
			wantErr: false,
		},
		{
			name:    "priority 10 is ok",
			mutate:  func(c *Config) { c.Providers.Grafana.Priority = 10 },
			wantErr: false,
		},
		{
			name:    "priority above 10 errors",
			mutate:  func(c *Config) { c.Providers.Grafana.Priority = 11 },
			wantErr: true,
		},
		{
			name:    "negative priority errors",
			mutate:  func(c *Config) { c.Providers.Grafana.Priority = -1 },
			wantErr: true,
		},
		{
			// Priorities are validated for every provider regardless of
			// Enabled, so an out-of-range disabled provider is still rejected.
			name: "out-of-range priority on disabled provider errors",
			mutate: func(c *Config) {
				c.Providers.Gatus.Enabled = false
				c.Providers.Gatus.Priority = 99
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(cfg)
			err := cfg.validatePriorities()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validatePriorities() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
