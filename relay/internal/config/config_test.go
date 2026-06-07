package config

import (
	"testing"
	"time"
)

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
