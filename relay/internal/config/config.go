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
	TrustedProxyCIDRs []string                  `yaml:"trusted_proxy_cidrs"`
	Providers         ProvidersConfig           `yaml:"providers"`
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

// GrafanaConfig holds Grafana-specific settings.
type GrafanaConfig struct {
	Enabled         bool          `yaml:"enabled"`
	SeverityLabel   string        `yaml:"severity_label"`
	DefaultSeverity string        `yaml:"default_severity"`
	DefaultIcon     string        `yaml:"default_icon"`
	Priority        int           `yaml:"priority"`
	CleanupDelay    time.Duration `yaml:"cleanup_delay"`
	StaleTimeout    time.Duration `yaml:"stale_timeout"`
}

// ArgoCDConfig holds ArgoCD-specific settings.
type ArgoCDConfig struct {
	Enabled         bool          `yaml:"enabled"`
	URL             string        `yaml:"url"`
	SyncGracePeriod time.Duration `yaml:"sync_grace_period"`
	Priority        int           `yaml:"priority"`
	CleanupDelay    time.Duration `yaml:"cleanup_delay"`
	StaleTimeout    time.Duration `yaml:"stale_timeout"`
	EndDelay        time.Duration `yaml:"end_delay"`
	EndDisplayTime  time.Duration `yaml:"end_display_time"`
}

// StarrConfig holds Radarr/Sonarr-specific settings.
//
// In the relay, Radarr/Sonarr send the hlk_ integration key as the Basic Auth
// password (extracted by the relay auth middleware). The username field is ignored.
type StarrConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Priority       int           `yaml:"priority"`
	CleanupDelay   time.Duration `yaml:"cleanup_delay"`
	StaleTimeout   time.Duration `yaml:"stale_timeout"`
	EndDelay       time.Duration `yaml:"end_delay"`
	EndDisplayTime time.Duration `yaml:"end_display_time"`
}

// JellyfinConfig holds Jellyfin-specific settings.
type JellyfinConfig struct {
	Enabled          bool          `yaml:"enabled"`
	Priority         int           `yaml:"priority"`
	CleanupDelay     time.Duration `yaml:"cleanup_delay"`
	StaleTimeout     time.Duration `yaml:"stale_timeout"`
	EndDelay         time.Duration `yaml:"end_delay"`
	EndDisplayTime   time.Duration `yaml:"end_display_time"`
	ProgressDebounce time.Duration `yaml:"progress_debounce"`
	PauseTimeout     time.Duration `yaml:"pause_timeout"`
}

// PaperlessConfig holds Paperless-ngx-specific settings.
type PaperlessConfig struct {
	Enabled      bool          `yaml:"enabled"`
	Priority     int           `yaml:"priority"`
	CleanupDelay time.Duration `yaml:"cleanup_delay"`
	StaleTimeout time.Duration `yaml:"stale_timeout"`
	EndDelay     time.Duration `yaml:"end_delay"`
	EndDisplayTime time.Duration `yaml:"end_display_time"`
}

// ChangedetectionConfig holds Changedetection.io-specific settings.
type ChangedetectionConfig struct {
	Enabled      bool          `yaml:"enabled"`
	Priority     int           `yaml:"priority"`
	CleanupDelay time.Duration `yaml:"cleanup_delay"`
	StaleTimeout time.Duration `yaml:"stale_timeout"`
}

// UnmanicConfig holds Unmanic-specific settings.
type UnmanicConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Priority       int           `yaml:"priority"`
	CleanupDelay   time.Duration `yaml:"cleanup_delay"`
	StaleTimeout   time.Duration `yaml:"stale_timeout"`
	EndDelay       time.Duration `yaml:"end_delay"`
	EndDisplayTime time.Duration `yaml:"end_display_time"`
}

// ProxmoxConfig holds Proxmox VE-specific settings.
type ProxmoxConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Priority       int           `yaml:"priority"`
	CleanupDelay   time.Duration `yaml:"cleanup_delay"`
	StaleTimeout   time.Duration `yaml:"stale_timeout"`
	EndDelay       time.Duration `yaml:"end_delay"`
	EndDisplayTime time.Duration `yaml:"end_display_time"`
}

// OverseerrConfig holds Overseerr/Jellyseerr-specific settings.
type OverseerrConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Priority       int           `yaml:"priority"`
	CleanupDelay   time.Duration `yaml:"cleanup_delay"`
	StaleTimeout   time.Duration `yaml:"stale_timeout"`
	EndDelay       time.Duration `yaml:"end_delay"`
	EndDisplayTime time.Duration `yaml:"end_display_time"`
}

// BackrestConfig holds Backrest-specific settings.
type BackrestConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Priority       int           `yaml:"priority"`
	CleanupDelay   time.Duration `yaml:"cleanup_delay"`
	StaleTimeout   time.Duration `yaml:"stale_timeout"`
	EndDelay       time.Duration `yaml:"end_delay"`
	EndDisplayTime time.Duration `yaml:"end_display_time"`
}

// GatusConfig holds Gatus-specific settings.
type GatusConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Priority       int           `yaml:"priority"`
	CleanupDelay   time.Duration `yaml:"cleanup_delay"`
	StaleTimeout   time.Duration `yaml:"stale_timeout"`
	EndDelay       time.Duration `yaml:"end_delay"`
	EndDisplayTime time.Duration `yaml:"end_display_time"`
}

// UptimeKumaConfig holds Uptime Kuma-specific settings.
type UptimeKumaConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Priority       int           `yaml:"priority"`
	CleanupDelay   time.Duration `yaml:"cleanup_delay"`
	StaleTimeout   time.Duration `yaml:"stale_timeout"`
	EndDelay       time.Duration `yaml:"end_delay"`
	EndDisplayTime time.Duration `yaml:"end_display_time"`
}

// Load reads the config from a YAML file and applies environment variable overrides.
func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: sharedconfig.ServerConfig{
			Address: ":8090",
		},
		Providers: ProvidersConfig{
			Grafana: GrafanaConfig{
				Enabled:         true,
				SeverityLabel:   "severity",
				DefaultSeverity: "warning",
				DefaultIcon:     "exclamationmark.triangle.fill",
				Priority:        5,
				CleanupDelay:    15 * time.Minute,
				StaleTimeout:    24 * time.Hour,
			},
			ArgoCD: ArgoCDConfig{
				Enabled:         true,
				SyncGracePeriod: 10 * time.Second,
				Priority:        3,
				CleanupDelay:    15 * time.Minute,
				StaleTimeout:    30 * time.Minute,
				EndDelay:        5 * time.Second,
				EndDisplayTime:  4 * time.Second,
			},
			Starr: StarrConfig{
				Enabled:        true,
				Priority:       1,
				CleanupDelay:   15 * time.Minute,
				StaleTimeout:   30 * time.Minute,
				EndDelay:       5 * time.Second,
				EndDisplayTime: 4 * time.Second,
			},
			Jellyfin: JellyfinConfig{
				Enabled:          true,
				Priority:         1,
				CleanupDelay:     15 * time.Minute,
				StaleTimeout:     30 * time.Minute,
				EndDelay:         5 * time.Second,
				EndDisplayTime:   4 * time.Second,
				ProgressDebounce: 10 * time.Second,
				PauseTimeout:     5 * time.Minute,
			},
			Paperless: PaperlessConfig{
				Enabled:        true,
				Priority:       1,
				CleanupDelay:   15 * time.Minute,
				StaleTimeout:   30 * time.Minute,
				EndDelay:       5 * time.Second,
				EndDisplayTime: 4 * time.Second,
			},
			Changedetection: ChangedetectionConfig{
				Enabled:      true,
				Priority:     2,
				CleanupDelay: 15 * time.Minute,
				StaleTimeout: 1 * time.Hour,
			},
			Unmanic: UnmanicConfig{
				Enabled:        true,
				Priority:       1,
				CleanupDelay:   15 * time.Minute,
				StaleTimeout:   30 * time.Minute,
				EndDelay:       5 * time.Second,
				EndDisplayTime: 4 * time.Second,
			},
			Proxmox: ProxmoxConfig{
				Enabled:        true,
				Priority:       4,
				CleanupDelay:   15 * time.Minute,
				StaleTimeout:   1 * time.Hour,
				EndDelay:       5 * time.Second,
				EndDisplayTime: 4 * time.Second,
			},
			Overseerr: OverseerrConfig{
				Enabled:        true,
				Priority:       1,
				CleanupDelay:   15 * time.Minute,
				StaleTimeout:   30 * time.Minute,
				EndDelay:       5 * time.Second,
				EndDisplayTime: 4 * time.Second,
			},
			UptimeKuma: UptimeKumaConfig{
				Enabled:        true,
				Priority:       5,
				CleanupDelay:   15 * time.Minute,
				StaleTimeout:   24 * time.Hour,
				EndDelay:       5 * time.Second,
				EndDisplayTime: 4 * time.Second,
			},
			Gatus: GatusConfig{
				Enabled:        true,
				Priority:       5,
				CleanupDelay:   15 * time.Minute,
				StaleTimeout:   24 * time.Hour,
				EndDelay:       5 * time.Second,
				EndDisplayTime: 4 * time.Second,
			},
			Backrest: BackrestConfig{
				Enabled:        true,
				Priority:       2,
				CleanupDelay:   15 * time.Minute,
				StaleTimeout:   1 * time.Hour,
				EndDelay:       5 * time.Second,
				EndDisplayTime: 4 * time.Second,
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
	if v := os.Getenv("PUSHWARD_SYNC_GRACE_PERIOD"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parsing PUSHWARD_SYNC_GRACE_PERIOD: %w", err)
		}
		cfg.Providers.ArgoCD.SyncGracePeriod = d
	}

	return nil
}
