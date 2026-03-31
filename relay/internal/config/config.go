package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

// Config holds the relay gateway configuration.
type Config struct {
	Server            sharedconfig.ServerConfig `yaml:"server"`
	Database          DatabaseConfig            `yaml:"database"`
	Telemetry         TelemetryConfig           `yaml:"telemetry"`
	TrustedProxyCIDRs []string                  `yaml:"trusted_proxy_cidrs"`
	Providers         ProvidersConfig           `yaml:"providers"`
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
	Proxmox         ProxmoxConfig         `yaml:"proxmox"`
	Overseerr       OverseerrConfig       `yaml:"overseerr"`
	UptimeKuma      UptimeKumaConfig      `yaml:"uptimekuma"`
	Gatus           GatusConfig           `yaml:"gatus"`
	Backrest        BackrestConfig        `yaml:"backrest"`
}

// BaseProviderConfig holds fields shared by all provider configs.
type BaseProviderConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Priority       int           `yaml:"priority"`
	CleanupDelay   time.Duration `yaml:"cleanup_delay"`
	StaleTimeout   time.Duration `yaml:"stale_timeout"`
	EndDelay       time.Duration `yaml:"end_delay"`
	EndDisplayTime time.Duration `yaml:"end_display_time"`
}

// GrafanaConfig holds Grafana-specific settings.
// Grafana alerts are fire-and-forget (no two-phase end), so EndDelay and
// EndDisplayTime from BaseProviderConfig are unused.
type GrafanaConfig struct {
	BaseProviderConfig `yaml:",inline"`
	SeverityLabel      string `yaml:"severity_label"`
	DefaultSeverity    string `yaml:"default_severity"`
	DefaultIcon        string `yaml:"default_icon"`
}

// ArgoCDConfig holds ArgoCD-specific settings.
type ArgoCDConfig struct {
	BaseProviderConfig `yaml:",inline"`
	URL                string        `yaml:"url"`
	SyncGracePeriod    time.Duration `yaml:"sync_grace_period"`
}

// StarrConfig holds Radarr/Sonarr-specific settings.
//
// In the relay, Radarr/Sonarr send the hlk_ integration key as the Basic Auth
// password (extracted by the relay auth middleware). The username field is ignored.
type StarrConfig struct {
	BaseProviderConfig `yaml:",inline"`
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

// Load reads the config from a YAML file and applies environment variable overrides.
func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: sharedconfig.ServerConfig{
			Address: ":8090",
		},
		Providers: ProvidersConfig{
			Grafana: GrafanaConfig{
				BaseProviderConfig: BaseProviderConfig{
					Enabled:      true,
					Priority:     5,
					CleanupDelay: 15 * time.Minute,
					StaleTimeout: 24 * time.Hour,
				},
				SeverityLabel:   "severity",
				DefaultSeverity: "warning",
				DefaultIcon:     "exclamationmark.triangle.fill",
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
		},
	}

	if err := sharedconfig.LoadYAML(path, cfg); err != nil {
		return nil, err
	}

	if err := cfg.applyEnvOverrides(); err != nil {
		return nil, err
	}

	if cfg.Database.DSN == "" {
		return nil, fmt.Errorf("database.dsn is required (set PUSHWARD_DATABASE_DSN)")
	}

	if err := cfg.validatePriorities(); err != nil {
		return nil, err
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
	if v := os.Getenv("PUSHWARD_OTEL_SAMPLE_RATE"); v != "" {
		rate, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("parsing PUSHWARD_OTEL_SAMPLE_RATE: %w", err)
		}
		cfg.Telemetry.SampleRate = rate
	}
	if v := os.Getenv("PUSHWARD_TRUSTED_PROXY_CIDRS"); v != "" {
		parts := strings.Split(v, ",")
		for i, p := range parts {
			parts[i] = strings.TrimSpace(p)
		}
		cfg.TrustedProxyCIDRs = parts
	}

	// Provider Enabled overrides
	if v := os.Getenv("PUSHWARD_GRAFANA_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("parsing PUSHWARD_GRAFANA_ENABLED: %w", err)
		}
		cfg.Providers.Grafana.Enabled = b
	}
	if v := os.Getenv("PUSHWARD_ARGOCD_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("parsing PUSHWARD_ARGOCD_ENABLED: %w", err)
		}
		cfg.Providers.ArgoCD.Enabled = b
	}
	if v := os.Getenv("PUSHWARD_STARR_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("parsing PUSHWARD_STARR_ENABLED: %w", err)
		}
		cfg.Providers.Starr.Enabled = b
	}
	// Grafana overrides
	if v := os.Getenv("PUSHWARD_GRAFANA_SEVERITY_LABEL"); v != "" {
		cfg.Providers.Grafana.SeverityLabel = v
	}
	if v := os.Getenv("PUSHWARD_GRAFANA_DEFAULT_SEVERITY"); v != "" {
		cfg.Providers.Grafana.DefaultSeverity = v
	}
	if v := os.Getenv("PUSHWARD_GRAFANA_DEFAULT_ICON"); v != "" {
		cfg.Providers.Grafana.DefaultIcon = v
	}

	// ArgoCD overrides
	if v := os.Getenv("PUSHWARD_ARGOCD_URL"); v != "" {
		cfg.Providers.ArgoCD.URL = v
	}
	// PUSHWARD_ARGOCD_SYNC_GRACE_PERIOD is the canonical name;
	// PUSHWARD_SYNC_GRACE_PERIOD is kept as a fallback for existing deployments.
	syncGrace := os.Getenv("PUSHWARD_ARGOCD_SYNC_GRACE_PERIOD")
	if syncGrace == "" {
		syncGrace = os.Getenv("PUSHWARD_SYNC_GRACE_PERIOD")
	}
	if syncGrace != "" {
		d, err := time.ParseDuration(syncGrace)
		if err != nil {
			return fmt.Errorf("parsing PUSHWARD_ARGOCD_SYNC_GRACE_PERIOD: %w", err)
		}
		cfg.Providers.ArgoCD.SyncGracePeriod = d
	}

	return nil
}

func (cfg *Config) validatePriorities() error {
	type entry struct {
		name     string
		priority int
	}
	providers := []entry{
		{"grafana", cfg.Providers.Grafana.Priority},
		{"argocd", cfg.Providers.ArgoCD.Priority},
		{"starr", cfg.Providers.Starr.Priority},
		{"jellyfin", cfg.Providers.Jellyfin.Priority},
		{"paperless", cfg.Providers.Paperless.Priority},
		{"changedetection", cfg.Providers.Changedetection.Priority},
		{"unmanic", cfg.Providers.Unmanic.Priority},
		{"proxmox", cfg.Providers.Proxmox.Priority},
		{"overseerr", cfg.Providers.Overseerr.Priority},
		{"uptimekuma", cfg.Providers.UptimeKuma.Priority},
		{"gatus", cfg.Providers.Gatus.Priority},
		{"backrest", cfg.Providers.Backrest.Priority},
	}
	for _, p := range providers {
		if p.priority < 0 || p.priority > 10 {
			return fmt.Errorf("providers.%s.priority: must be 0-10, got %d", p.name, p.priority)
		}
	}
	return nil
}
