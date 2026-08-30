package config

import (
	"reflect"
	"strings"
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
	"PUSHWARD_POSTER_ENABLED",
	"PUSHWARD_POSTER_ALLOW_PRIVATE_HOSTS",
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
// disabled zero values (which are valid - a disabled provider is never checked
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

	cfg.Poster = PosterConfig{
		FetchTimeout: 3 * time.Second,
		InlineWait:   600 * time.Millisecond,
	}
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

func TestApplyEnvOverrides_ProviderEnabled(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		startOn bool
		want    bool
		wantErr bool
	}{
		{name: "false turns an enabled provider off", env: "false", startOn: true},
		{name: "1 turns a disabled provider on", env: "1", want: true},
		{name: "unset leaves the default", env: "", startOn: true, want: true},
		{name: "an unparseable value is an error", env: "yes", startOn: true, wantErr: true},
	}

	// Every provider that has an _ENABLED override, paired with the field it sets.
	providers := map[string]func(*Config) *bool{
		"PUSHWARD_GRAFANA_ENABLED": func(c *Config) *bool { return &c.Providers.Grafana.Enabled },
		"PUSHWARD_ARGOCD_ENABLED":  func(c *Config) *bool { return &c.Providers.ArgoCD.Enabled },
		"PUSHWARD_STARR_ENABLED":   func(c *Config) *bool { return &c.Providers.Starr.Enabled },
		"PUSHWARD_GITEA_ENABLED":   func(c *Config) *bool { return &c.Providers.Gitea.Enabled },
	}

	for name, field := range providers {
		for _, tt := range tests {
			t.Run(name+"/"+tt.name, func(t *testing.T) {
				clearRelayEnv(t)
				t.Setenv(name, tt.env)

				cfg := &Config{}
				*field(cfg) = tt.startOn
				err := cfg.applyEnvOverrides()
				if tt.wantErr {
					if err == nil {
						t.Fatal("expected an error for an unparseable flag")
					}
					if !strings.Contains(err.Error(), name) {
						t.Errorf("error must name the variable, got %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("applyEnvOverrides() error = %v", err)
				}
				if got := *field(cfg); got != tt.want {
					t.Errorf("%s = %v, want %v", name, got, tt.want)
				}
			})
		}
	}
}

func TestApplyEnvOverrides_SyncGracePeriodFallback(t *testing.T) {
	const (
		canonical = "PUSHWARD_ARGOCD_SYNC_GRACE_PERIOD"
		legacy    = "PUSHWARD_SYNC_GRACE_PERIOD"
	)

	tests := []struct {
		name        string
		canonical   string
		legacy      string
		want        time.Duration
		wantErrName string
	}{
		{name: "canonical applies", canonical: "20s", want: 20 * time.Second},
		{name: "legacy applies when canonical is unset", legacy: "45s", want: 45 * time.Second},
		{name: "canonical wins when both are set", canonical: "20s", legacy: "45s", want: 20 * time.Second},
		{name: "neither set leaves the YAML value", want: 10 * time.Second},
		{
			// The canonical name is what was read, so it is what the error names.
			name:        "a malformed canonical value errors",
			canonical:   "20 seconds",
			wantErrName: canonical,
		},
		{
			// Only reachable when the canonical name is unset, so the error must
			// name the legacy variable rather than the one the operator never set.
			name:        "a malformed legacy value errors under its own name",
			legacy:      "45 seconds",
			wantErrName: legacy,
		},
		{
			// A stale malformed fallback must not block a boot the canonical name
			// has already fixed: the fallback is never parsed once canonical is set.
			name:      "a malformed legacy value is ignored when canonical is set",
			canonical: "20s",
			legacy:    "not a duration",
			want:      20 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearRelayEnv(t)
			t.Setenv(canonical, tt.canonical)
			t.Setenv(legacy, tt.legacy)

			cfg := &Config{}
			cfg.Providers.ArgoCD.SyncGracePeriod = 10 * time.Second
			err := cfg.applyEnvOverrides()
			if tt.wantErrName != "" {
				if err == nil {
					t.Fatal("expected an error for an unparseable duration")
				}
				if !strings.Contains(err.Error(), tt.wantErrName) {
					t.Errorf("error must name %s, got %v", tt.wantErrName, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("applyEnvOverrides() error = %v", err)
			}
			if got := cfg.Providers.ArgoCD.SyncGracePeriod; got != tt.want {
				t.Errorf("SyncGracePeriod = %v, want %v", got, tt.want)
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
			// A disabled argocd with a negative grace period must not error -
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

func TestValidatePoster(t *testing.T) {
	falseVal, trueVal := false, true
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{
			name:   "valid poster block passes",
			mutate: func(*Config) {},
		},
		{
			name:   "unset enabled is on and still validated",
			mutate: func(c *Config) { c.Poster.Enabled = nil },
		},
		{
			name:   "explicitly enabled",
			mutate: func(c *Config) { c.Poster.Enabled = &trueVal },
		},
		{
			// A relay that turned the feature off must boot even with a stale
			// or empty block underneath it.
			name:   "disabled skips validation entirely",
			mutate: func(c *Config) { c.Poster = PosterConfig{Enabled: &falseVal} },
		},
		{
			name:    "zero fetch timeout errors",
			mutate:  func(c *Config) { c.Poster.FetchTimeout = 0 },
			wantErr: true,
		},
		{
			name:    "zero inline wait errors",
			mutate:  func(c *Config) { c.Poster.InlineWait = 0 },
			wantErr: true,
		},
		{
			name:    "inline wait past the fetch timeout errors",
			mutate:  func(c *Config) { c.Poster.InlineWait = 5 * time.Second },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(cfg)
			err := cfg.validatePoster()
			if (err != nil) != tt.wantErr {
				t.Errorf("validatePoster() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// An absent `enabled` key leaves posters on; only an explicit false turns them
// off, which is what the *bool is for.
func TestPosterIsEnabled(t *testing.T) {
	falseVal, trueVal := false, true
	if !(PosterConfig{}).IsEnabled() {
		t.Error("an unset enabled must read as on")
	}
	if !(PosterConfig{Enabled: &trueVal}).IsEnabled() {
		t.Error("enabled: true must read as on")
	}
	if (PosterConfig{Enabled: &falseVal}).IsEnabled() {
		t.Error("enabled: false must read as off")
	}
}

func TestApplyEnvOverrides_Poster(t *testing.T) {
	t.Run("env can turn posters off", func(t *testing.T) {
		clearRelayEnv(t)
		t.Setenv("PUSHWARD_POSTER_ENABLED", "false")
		cfg := validConfig()
		if err := cfg.applyEnvOverrides(); err != nil {
			t.Fatalf("applyEnvOverrides: %v", err)
		}
		if cfg.Poster.IsEnabled() {
			t.Error("expected posters to be disabled")
		}
	})

	t.Run("env can allow private hosts", func(t *testing.T) {
		clearRelayEnv(t)
		t.Setenv("PUSHWARD_POSTER_ALLOW_PRIVATE_HOSTS", "true")
		cfg := validConfig()
		if err := cfg.applyEnvOverrides(); err != nil {
			t.Fatalf("applyEnvOverrides: %v", err)
		}
		if !cfg.Poster.AllowPrivateHosts {
			t.Error("expected allow_private_hosts to be on")
		}
	})

	t.Run("private hosts stay off by default", func(t *testing.T) {
		clearRelayEnv(t)
		cfg := validConfig()
		if err := cfg.applyEnvOverrides(); err != nil {
			t.Fatalf("applyEnvOverrides: %v", err)
		}
		if cfg.Poster.AllowPrivateHosts {
			t.Error("allow_private_hosts must stay off unless asked for")
		}
		if !cfg.Poster.IsEnabled() {
			t.Error("posters must stay on by default")
		}
	})
}

// The dismissal_ttl defaults are a policy decision, not an accident, and without
// a test the Tier C half erodes silently: a completion confirmation clears the
// Lock Screen after two minutes, while an alert or a build result - the cards
// you actually come back to read - keep the server default.
func TestDefaultDismissalDelay(t *testing.T) {
	clearRelayEnv(t)
	t.Setenv("PUSHWARD_DATABASE_DSN", "postgres://relay@localhost/relay")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Providers

	completions := map[string]*time.Duration{
		"starr":     p.Starr.DismissalDelay,
		"paperless": p.Paperless.DismissalDelay,
		"unmanic":   p.Unmanic.DismissalDelay,
		"overseerr": p.Overseerr.DismissalDelay,
	}
	for name, got := range completions {
		if got == nil {
			t.Errorf("%s: expected a dismissal_delay default, got nil", name)
			continue
		}
		if *got != 2*time.Minute {
			t.Errorf("%s: expected 2m dismissal_delay, got %v", name, *got)
		}
	}

	// Each provider must own its pointer: yaml.v3 decodes into a non-nil pointer
	// in place, so a shared address would let one provider's dismissal_delay
	// rewrite every other provider's default.
	if p.Starr.DismissalDelay == p.Paperless.DismissalDelay {
		t.Error("providers share one *time.Duration; a YAML override on either would move both")
	}

	glanceable := map[string]*time.Duration{
		"grafana":         p.Grafana.DismissalDelay,
		"argocd":          p.ArgoCD.DismissalDelay,
		"uptimekuma":      p.UptimeKuma.DismissalDelay,
		"gatus":           p.Gatus.DismissalDelay,
		"proxmox":         p.Proxmox.DismissalDelay,
		"truenas":         p.TrueNAS.DismissalDelay,
		"komodo":          p.Komodo.DismissalDelay,
		"changedetection": p.Changedetection.DismissalDelay,
		"backrest":        p.Backrest.DismissalDelay,
		"gitea":           p.Gitea.DismissalDelay,
		"jellyfin":        p.Jellyfin.DismissalDelay,
	}
	for name, got := range glanceable {
		if got != nil {
			t.Errorf("%s: expected no dismissal_delay (the card is worth coming back to), got %v", name, *got)
		}
	}
}
