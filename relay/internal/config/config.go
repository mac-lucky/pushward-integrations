package config

import (
	"fmt"
	"os"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/poster"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// Config holds the relay gateway configuration.
type Config struct {
	Server            sharedconfig.ServerConfig `yaml:"server"`
	Database          DatabaseConfig            `yaml:"database"`
	Telemetry         TelemetryConfig           `yaml:"telemetry"`
	CircuitBreaker    CircuitBreakerConfig      `yaml:"circuit_breaker"`
	Poster            PosterConfig              `yaml:"poster"`
	TrustedProxyCIDRs []string                  `yaml:"trusted_proxy_cidrs"`
	Providers         ProvidersConfig           `yaml:"providers"`
}

// PosterConfig controls activity poster images: the image_url / image_shape /
// image_thumbhash trio the Starr, Jellyfin and Overseerr handlers attach to
// their cards. The response cap, cache size and negative TTL are deliberately
// not settable - they are memory limits on a multi-tenant process, not
// per-deployment taste.
type PosterConfig struct {
	// Enabled is a pointer so an explicit `enabled: false` is distinguishable
	// from an absent key, which stays on. Read it through IsEnabled.
	Enabled *bool `yaml:"enabled"`
	// AllowPrivateHosts lets thumbhash fetches reach LAN addresses over
	// cleartext http. Off by default: the hosted relay's webhook payloads are
	// attacker-controlled, so an unguarded fetcher is an SSRF probe of the
	// cluster. Self-hosted relays that want artwork off a LAN Jellyfin turn it
	// on.
	AllowPrivateHosts bool          `yaml:"allow_private_hosts"`
	FetchTimeout      time.Duration `yaml:"fetch_timeout"`
	InlineWait        time.Duration `yaml:"inline_wait"`
}

// IsEnabled reads Enabled, treating an absent key as on.
func (p PosterConfig) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// CircuitBreakerConfig controls the circuit breaker for outbound PushWard API calls.
type CircuitBreakerConfig struct {
	Threshold int           `yaml:"threshold"` // Consecutive failures to open (default 5).
	Cooldown  time.Duration `yaml:"cooldown"`  // How long to stay open (default 30s).
}

// TelemetryConfig holds OpenTelemetry tracing configuration.
type TelemetryConfig struct {
	Endpoint    string  `yaml:"endpoint"`      // OTLP gRPC endpoint (e.g. "traces.example.com:443"). Empty disables telemetry.
	TLSCertPath string  `yaml:"tls_cert_path"` // Client certificate PEM for mTLS.
	TLSKeyPath  string  `yaml:"tls_key_path"`  // Client private key PEM for mTLS.
	SampleRate  float64 `yaml:"sample_rate"`   // Sampling rate 0.0-1.0 (default: 1.0).
}

// DatabaseConfig holds the PostgreSQL connection settings.
type DatabaseConfig struct {
	DSN          string `yaml:"dsn"`
	PasswordFile string `yaml:"password_file"`
}

// ProvidersConfig holds per-provider settings.
type ProvidersConfig struct {
	Grafana         GrafanaConfig         `yaml:"grafana"`
	ArgoCD          ArgoCDConfig          `yaml:"argocd"`
	Starr           StarrConfig           `yaml:"starr"`
	Jellyfin        JellyfinConfig        `yaml:"jellyfin"`
	Paperless       PaperlessConfig       `yaml:"paperless"`
	Changedetection ChangedetectionConfig `yaml:"changedetection"`
	Unmanic         UnmanicConfig         `yaml:"unmanic"`
	Bazarr          BazarrConfig          `yaml:"bazarr"`
	Proxmox         ProxmoxConfig         `yaml:"proxmox"`
	Overseerr       OverseerrConfig       `yaml:"overseerr"`
	UptimeKuma      UptimeKumaConfig      `yaml:"uptimekuma"`
	Gatus           GatusConfig           `yaml:"gatus"`
	Backrest        BackrestConfig        `yaml:"backrest"`
	Gitea           GiteaConfig           `yaml:"gitea"`
	Komodo          KomodoConfig          `yaml:"komodo"`
	TrueNAS         TrueNASConfig         `yaml:"truenas"`
}

// BaseProviderConfig holds fields shared by all provider configs.
type BaseProviderConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Priority       int           `yaml:"priority"`
	CleanupDelay   time.Duration `yaml:"cleanup_delay"`
	StaleTimeout   time.Duration `yaml:"stale_timeout"`
	EndDelay       time.Duration `yaml:"end_delay"`
	EndDisplayTime time.Duration `yaml:"end_display_time"`

	// DismissalDelay maps to the server's dismissal_ttl: how long an ENDED card
	// stays on the Lock Screen, which CleanupDelay (deletion) otherwise decides.
	// Nil leaves the server default. A pointer because 0 means "remove
	// immediately", which is a real setting rather than an unset one.
	DismissalDelay *time.Duration `yaml:"dismissal_delay"`
}

// CreateOptions turns the configured dismissal delay into CreateActivity
// options, so a handler can splat it without a nil check.
func (b BaseProviderConfig) CreateOptions() []pushward.CreateOption {
	return pushward.DismissalTTLOptions(b.DismissalDelay)
}

// completionDismissalDelay returns the default dismissal_ttl for the providers whose
// terminal event is a completion confirmation rather than something to come
// back to: paperless, unmanic, overseerr and the two Starr apps. Each of them
// also fires a notification on the same path, which is the durable record, and
// they are the highest-churn family in the relay - a Sonarr season pack fires
// once per episode. Alert and build/backup providers deliberately leave it
// unset: a resolved outage or a finished build is exactly the card you go back
// to the Lock Screen for. Override per provider with dismissal_delay.
//
// A function, not a shared var: yaml.v3 decodes into a non-nil pointer in place,
// so one shared address would let a dismissal_delay on any single provider
// rewrite the default for all four.
func completionDismissalDelay() *time.Duration {
	return pushward.DurationPtr(2 * time.Minute)
}

// GrafanaConfig holds Grafana-specific settings.
// Grafana alerts are fire-and-forget (no two-phase end), so EndDelay and
// EndDisplayTime from BaseProviderConfig are unused.
type GrafanaConfig struct {
	BaseProviderConfig `yaml:",inline"`
}

// ArgoCDConfig holds ArgoCD-specific settings.
type ArgoCDConfig struct {
	BaseProviderConfig `yaml:",inline"`
	URL                string        `yaml:"url"`
	SyncGracePeriod    time.Duration `yaml:"sync_grace_period"`
}

// NotificationMode controls how a provider routes events.
type NotificationMode string

const (
	ModeActivity NotificationMode = "activity" // All events -> Live Activity (default, current behavior)
	ModeNotify   NotificationMode = "notify"   // All events -> push notification
	ModeSmart    NotificationMode = "smart"    // Handler decides per event type
)

// StarrConfig holds Radarr/Sonarr-specific settings.
//
// In the relay, Radarr/Sonarr send the hlk_ integration key as the Basic Auth
// password (extracted by the relay auth middleware). The username field is ignored.
type StarrConfig struct {
	BaseProviderConfig `yaml:",inline"`
	Mode               NotificationMode `yaml:"mode"`
}

// JellyfinConfig holds Jellyfin-specific settings.
type JellyfinConfig struct {
	BaseProviderConfig `yaml:",inline"`
	ProgressDebounce   time.Duration `yaml:"progress_debounce"`
	PauseTimeout       time.Duration `yaml:"pause_timeout"`
}

// PaperlessConfig holds Paperless-ngx-specific settings.
type PaperlessConfig struct {
	BaseProviderConfig `yaml:",inline"`
}

// ChangedetectionConfig holds Changedetection.io-specific settings.
// Changedetection alerts are fire-and-forget, so EndDelay and
// EndDisplayTime from BaseProviderConfig are unused.
type ChangedetectionConfig struct {
	BaseProviderConfig `yaml:",inline"`
}

// UnmanicConfig holds Unmanic-specific settings.
type UnmanicConfig struct {
	BaseProviderConfig `yaml:",inline"`
}

// BazarrConfig holds Bazarr-specific settings.
type BazarrConfig struct {
	BaseProviderConfig `yaml:",inline"`
}

// ProxmoxConfig holds Proxmox VE-specific settings.
type ProxmoxConfig struct {
	BaseProviderConfig `yaml:",inline"`
}

// OverseerrConfig holds Overseerr/Jellyseerr-specific settings.
type OverseerrConfig struct {
	BaseProviderConfig `yaml:",inline"`
}

// BackrestConfig holds Backrest-specific settings.
type BackrestConfig struct {
	BaseProviderConfig `yaml:",inline"`
}

// GatusConfig holds Gatus-specific settings.
type GatusConfig struct {
	BaseProviderConfig `yaml:",inline"`
}

// UptimeKumaConfig holds Uptime Kuma-specific settings.
type UptimeKumaConfig struct {
	BaseProviderConfig `yaml:",inline"`
}

// GiteaConfig holds Gitea/Forgejo Actions-webhook settings. The single config
// backs both the /gitea and /forgejo routes.
//
// StaleTimeout defaults higher than most providers (4h): a single long-running
// job emits no webhook between its in_progress and completed events, so a 30m
// TTL would evict the run state mid-build.
type GiteaConfig struct {
	BaseProviderConfig `yaml:",inline"`
}

// KomodoConfig holds Komodo Custom-alerter settings.
type KomodoConfig struct {
	BaseProviderConfig `yaml:",inline"`
}

// TrueNASConfig holds TrueNAS OpsGenie-alert-service settings. StaleTimeout
// defaults high (24h): a long-lived alert that TrueNAS never clears is ended
// server-side at the timeout, and a later DELETE no-ops.
type TrueNASConfig struct {
	BaseProviderConfig `yaml:",inline"`
}

// Load reads the config from a YAML file and applies environment variable overrides.
func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: sharedconfig.ServerConfig{
			Address:        ":8090",
			MetricsAddress: ":9090",
		},
		CircuitBreaker: CircuitBreakerConfig{
			Threshold: 5,
			Cooldown:  30 * time.Second,
		},
		Poster: PosterConfig{
			FetchTimeout: poster.DefaultFetchTimeout,
			InlineWait:   poster.DefaultInlineWait,
		},
		// SampleRate default lives here (not in telemetry.Init) so an explicit
		// sample_rate: 0 in YAML can mean "sample nothing" while an unset value
		// keeps the 1.0 default.
		Telemetry: TelemetryConfig{SampleRate: 1.0},
		Providers: ProvidersConfig{
			Grafana: GrafanaConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:      true,
					Priority:     10,
					CleanupDelay: 15 * time.Minute,
					StaleTimeout: 24 * time.Hour,
				},
			},
			ArgoCD: ArgoCDConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:        true,
					Priority:       3,
					CleanupDelay:   15 * time.Minute,
					StaleTimeout:   30 * time.Minute,
					EndDelay:       5 * time.Second,
					EndDisplayTime: 4 * time.Second,
				},
				SyncGracePeriod: 10 * time.Second,
			},
			Starr: StarrConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:        true,
					Priority:       1,
					CleanupDelay:   15 * time.Minute,
					StaleTimeout:   30 * time.Minute,
					EndDelay:       5 * time.Second,
					EndDisplayTime: 4 * time.Second,
					DismissalDelay: completionDismissalDelay(),
				},
			},
			Jellyfin: JellyfinConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:        true,
					Priority:       1,
					CleanupDelay:   15 * time.Minute,
					StaleTimeout:   30 * time.Minute,
					EndDelay:       5 * time.Second,
					EndDisplayTime: 4 * time.Second,
				},
				ProgressDebounce: 10 * time.Second,
				PauseTimeout:     5 * time.Minute,
			},
			Paperless: PaperlessConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:        true,
					Priority:       1,
					CleanupDelay:   15 * time.Minute,
					StaleTimeout:   30 * time.Minute,
					EndDelay:       5 * time.Second,
					EndDisplayTime: 4 * time.Second,
					DismissalDelay: completionDismissalDelay(),
				},
			},
			Changedetection: ChangedetectionConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:      true,
					Priority:     2,
					CleanupDelay: 15 * time.Minute,
					StaleTimeout: 1 * time.Hour,
				},
			},
			Unmanic: UnmanicConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:        true,
					Priority:       1,
					CleanupDelay:   15 * time.Minute,
					StaleTimeout:   30 * time.Minute,
					EndDelay:       5 * time.Second,
					EndDisplayTime: 4 * time.Second,
					DismissalDelay: completionDismissalDelay(),
				},
			},
			Bazarr: BazarrConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:        true,
					Priority:       1,
					CleanupDelay:   15 * time.Minute,
					StaleTimeout:   30 * time.Minute,
					EndDelay:       5 * time.Second,
					EndDisplayTime: 4 * time.Second,
				},
			},
			Proxmox: ProxmoxConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:        true,
					Priority:       4,
					CleanupDelay:   15 * time.Minute,
					StaleTimeout:   1 * time.Hour,
					EndDelay:       5 * time.Second,
					EndDisplayTime: 4 * time.Second,
				},
			},
			Overseerr: OverseerrConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:        true,
					Priority:       1,
					CleanupDelay:   15 * time.Minute,
					StaleTimeout:   30 * time.Minute,
					EndDelay:       5 * time.Second,
					EndDisplayTime: 4 * time.Second,
					DismissalDelay: completionDismissalDelay(),
				},
			},
			UptimeKuma: UptimeKumaConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:        true,
					Priority:       5,
					CleanupDelay:   15 * time.Minute,
					StaleTimeout:   24 * time.Hour,
					EndDelay:       5 * time.Second,
					EndDisplayTime: 4 * time.Second,
				},
			},
			Gatus: GatusConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:        true,
					Priority:       5,
					CleanupDelay:   15 * time.Minute,
					StaleTimeout:   24 * time.Hour,
					EndDelay:       5 * time.Second,
					EndDisplayTime: 4 * time.Second,
				},
			},
			Backrest: BackrestConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:        true,
					Priority:       2,
					CleanupDelay:   15 * time.Minute,
					StaleTimeout:   1 * time.Hour,
					EndDelay:       5 * time.Second,
					EndDisplayTime: 4 * time.Second,
				},
			},
			Gitea: GiteaConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:        true,
					Priority:       3,
					CleanupDelay:   15 * time.Minute,
					StaleTimeout:   4 * time.Hour,
					EndDelay:       5 * time.Second,
					EndDisplayTime: 4 * time.Second,
				},
			},
			Komodo: KomodoConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:        true,
					Priority:       5,
					CleanupDelay:   15 * time.Minute,
					StaleTimeout:   24 * time.Hour,
					EndDelay:       5 * time.Second,
					EndDisplayTime: 4 * time.Second,
				},
			},
			TrueNAS: TrueNASConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:        true,
					Priority:       5,
					CleanupDelay:   15 * time.Minute,
					StaleTimeout:   24 * time.Hour,
					EndDelay:       5 * time.Second,
					EndDisplayTime: 4 * time.Second,
				},
			},
		},
	}

	if err := sharedconfig.LoadYAML(path, cfg); err != nil {
		return nil, err
	}

	if err := cfg.applyEnvOverrides(); err != nil {
		return nil, err
	}

	if cfg.Server.MetricsAddress != "" && cfg.Server.MetricsAddress == cfg.Server.Address {
		return nil, fmt.Errorf("server.metrics_address (%s) must differ from server.address (%s)",
			cfg.Server.MetricsAddress, cfg.Server.Address)
	}

	if cfg.Database.DSN == "" {
		return nil, fmt.Errorf("database.dsn is required (set PUSHWARD_DATABASE_DSN)")
	}

	if err := cfg.validateProviderTimeouts(); err != nil {
		return nil, err
	}

	if err := cfg.validatePriorities(); err != nil {
		return nil, err
	}

	if err := cfg.validateModes(); err != nil {
		return nil, err
	}

	if err := cfg.validatePoster(); err != nil {
		return nil, err
	}

	if cfg.CircuitBreaker.Threshold < 1 {
		return nil, fmt.Errorf("circuit_breaker.threshold must be >= 1, got %d", cfg.CircuitBreaker.Threshold)
	}
	if cfg.CircuitBreaker.Cooldown < time.Second {
		return nil, fmt.Errorf("circuit_breaker.cooldown must be >= 1s, got %s", cfg.CircuitBreaker.Cooldown)
	}

	return cfg, nil
}

func (cfg *Config) applyEnvOverrides() error {
	cfg.Server.ApplyEnvOverrides()

	if v := os.Getenv("PUSHWARD_DATABASE_DSN"); v != "" {
		cfg.Database.DSN = v
	}
	if v := os.Getenv("PUSHWARD_DATABASE_PASSWORD_FILE"); v != "" {
		cfg.Database.PasswordFile = v
	}

	// Telemetry overrides
	if v := os.Getenv("PUSHWARD_OTEL_ENDPOINT"); v != "" {
		cfg.Telemetry.Endpoint = v
	}
	if v := os.Getenv("PUSHWARD_OTEL_TLS_CERT_PATH"); v != "" {
		cfg.Telemetry.TLSCertPath = v
	}
	if v := os.Getenv("PUSHWARD_OTEL_TLS_KEY_PATH"); v != "" {
		cfg.Telemetry.TLSKeyPath = v
	}
	if err := sharedconfig.EnvFloat64("PUSHWARD_OTEL_SAMPLE_RATE", &cfg.Telemetry.SampleRate); err != nil {
		return err
	}
	if v := os.Getenv("PUSHWARD_TRUSTED_PROXY_CIDRS"); v != "" {
		cfg.TrustedProxyCIDRs = sharedconfig.SplitList(v)
	}

	// Poster overrides. Only the two operational switches get env names; the
	// timing and size knobs stay YAML-only, as the per-provider ones do.
	posterEnabled := cfg.Poster.IsEnabled()
	if err := sharedconfig.EnvBool("PUSHWARD_POSTER_ENABLED", &posterEnabled); err != nil {
		return err
	}
	cfg.Poster.Enabled = &posterEnabled
	if err := sharedconfig.EnvBool("PUSHWARD_POSTER_ALLOW_PRIVATE_HOSTS", &cfg.Poster.AllowPrivateHosts); err != nil {
		return err
	}

	// Provider Enabled overrides
	if err := sharedconfig.EnvBool("PUSHWARD_GRAFANA_ENABLED", &cfg.Providers.Grafana.Enabled); err != nil {
		return err
	}
	if err := sharedconfig.EnvBool("PUSHWARD_ARGOCD_ENABLED", &cfg.Providers.ArgoCD.Enabled); err != nil {
		return err
	}
	if err := sharedconfig.EnvBool("PUSHWARD_STARR_ENABLED", &cfg.Providers.Starr.Enabled); err != nil {
		return err
	}
	if v := os.Getenv("PUSHWARD_STARR_MODE"); v != "" {
		cfg.Providers.Starr.Mode = NotificationMode(v)
	}
	if err := sharedconfig.EnvBool("PUSHWARD_GITEA_ENABLED", &cfg.Providers.Gitea.Enabled); err != nil {
		return err
	}
	// ArgoCD overrides
	if v := os.Getenv("PUSHWARD_ARGOCD_URL"); v != "" {
		cfg.Providers.ArgoCD.URL = v
	}
	// PUSHWARD_ARGOCD_SYNC_GRACE_PERIOD is the canonical name;
	// PUSHWARD_SYNC_GRACE_PERIOD is kept as a fallback for existing deployments.
	// Resolve which name is in play before parsing: reading both would let a stale
	// malformed fallback fail a boot that the canonical name had already fixed.
	graceVar := "PUSHWARD_ARGOCD_SYNC_GRACE_PERIOD"
	if os.Getenv(graceVar) == "" {
		graceVar = "PUSHWARD_SYNC_GRACE_PERIOD"
	}
	if err := sharedconfig.EnvDuration(graceVar, &cfg.Providers.ArgoCD.SyncGracePeriod); err != nil {
		return err
	}

	return nil
}

// providerEntry pairs a provider's config-key name with its base config so the
// validators can iterate one source-of-truth list instead of duplicating the
// per-provider enumeration (adding a provider then touches only baseProviders).
type providerEntry struct {
	name string
	base BaseProviderConfig
}

// baseProviders returns every provider's name and BaseProviderConfig. It is the
// single list driving validateProviderTimeouts and validatePriorities.
func (cfg *Config) baseProviders() []providerEntry {
	return []providerEntry{
		{"grafana", cfg.Providers.Grafana.BaseProviderConfig},
		{"argocd", cfg.Providers.ArgoCD.BaseProviderConfig},
		{"starr", cfg.Providers.Starr.BaseProviderConfig},
		{"jellyfin", cfg.Providers.Jellyfin.BaseProviderConfig},
		{"paperless", cfg.Providers.Paperless.BaseProviderConfig},
		{"changedetection", cfg.Providers.Changedetection.BaseProviderConfig},
		{"unmanic", cfg.Providers.Unmanic.BaseProviderConfig},
		{"bazarr", cfg.Providers.Bazarr.BaseProviderConfig},
		{"proxmox", cfg.Providers.Proxmox.BaseProviderConfig},
		{"overseerr", cfg.Providers.Overseerr.BaseProviderConfig},
		{"uptimekuma", cfg.Providers.UptimeKuma.BaseProviderConfig},
		{"gatus", cfg.Providers.Gatus.BaseProviderConfig},
		{"backrest", cfg.Providers.Backrest.BaseProviderConfig},
		{"gitea", cfg.Providers.Gitea.BaseProviderConfig},
		{"komodo", cfg.Providers.Komodo.BaseProviderConfig},
		{"truenas", cfg.Providers.TrueNAS.BaseProviderConfig},
	}
}

// validateProviderTimeouts rejects a non-positive StaleTimeout for any enabled
// provider. A non-positive TTL makes the state store write rows with NULL
// expiry that the periodic Cleanup never deletes (postgres.go) - an unbounded
// table at 10k+ users.
func (cfg *Config) validateProviderTimeouts() error {
	for _, p := range cfg.baseProviders() {
		if p.base.Enabled && p.base.StaleTimeout <= 0 {
			return fmt.Errorf("providers.%s.stale_timeout must be > 0, got %s (a non-positive TTL writes state rows that are never cleaned up)", p.name, p.base.StaleTimeout)
		}
		// dismissal_ttl is clamped, not rejected, on the way to the server, so
		// an out-of-range value here fails silently in the worst direction: a
		// negative delay clamps to 0, which dismisses the card the instant it
		// ends - the opposite of what a longer window was asking for.
		if p.base.Enabled && p.base.DismissalDelay != nil {
			d := *p.base.DismissalDelay
			if maxDismissal := time.Duration(pushward.DismissalTTLMax) * time.Second; d < 0 || d > maxDismissal {
				return fmt.Errorf("providers.%s.dismissal_delay must be 0-%s, got %s", p.name, maxDismissal, d)
			}
		}
	}
	// ArgoCD feeds SyncGracePeriod*2 as a TTL, so a negative value would also
	// produce non-expiring rows.
	if cfg.Providers.ArgoCD.Enabled && cfg.Providers.ArgoCD.SyncGracePeriod < 0 {
		return fmt.Errorf("providers.argocd.sync_grace_period must be >= 0, got %s", cfg.Providers.ArgoCD.SyncGracePeriod)
	}
	return nil
}

func (cfg *Config) validatePriorities() error {
	for _, p := range cfg.baseProviders() {
		if p.base.Priority < 0 || p.base.Priority > 10 {
			return fmt.Errorf("providers.%s.priority: must be 0-10, got %d", p.name, p.base.Priority)
		}
	}
	return nil
}

// validatePoster rejects a poster block that would either never produce a hash
// or hold a webhook open. It only runs when the feature is on, so a deployment
// that turned it off is not blocked from booting by a stale value.
func (cfg *Config) validatePoster() error {
	if !cfg.Poster.IsEnabled() {
		return nil
	}
	p := cfg.Poster
	if p.FetchTimeout <= 0 {
		return fmt.Errorf("poster.fetch_timeout must be > 0, got %s", p.FetchTimeout)
	}
	if p.InlineWait <= 0 {
		return fmt.Errorf("poster.inline_wait must be > 0, got %s", p.InlineWait)
	}
	if p.InlineWait > p.FetchTimeout {
		return fmt.Errorf("poster.inline_wait (%s) must not exceed poster.fetch_timeout (%s): the extra wait can never be rewarded", p.InlineWait, p.FetchTimeout)
	}
	return nil
}

func (cfg *Config) validateModes() error {
	validModes := map[NotificationMode]bool{"": true, ModeActivity: true, ModeNotify: true, ModeSmart: true}
	if !validModes[cfg.Providers.Starr.Mode] {
		return fmt.Errorf("providers.starr.mode: must be activity, notify, or smart, got %q", cfg.Providers.Starr.Mode)
	}
	return nil
}
