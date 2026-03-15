package config

import (
	"fmt"
	"os"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"gopkg.in/yaml.v3"
)

// Config holds the relay gateway configuration.
type Config struct {
	Server   sharedconfig.ServerConfig `yaml:"server"`
	Database DatabaseConfig            `yaml:"database"`
	Providers ProvidersConfig          `yaml:"providers"`
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
}

// GrafanaConfig holds Grafana-specific settings.
type GrafanaConfig struct {
	Enabled         bool          `yaml:"enabled"`
	WebhookSecret   string        `yaml:"webhook_secret"`
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
	WebhookSecret   string        `yaml:"webhook_secret"`
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
	WebhookSecret    string        `yaml:"webhook_secret"`
	Priority         int           `yaml:"priority"`
	CleanupDelay     time.Duration `yaml:"cleanup_delay"`
	StaleTimeout     time.Duration `yaml:"stale_timeout"`
	EndDelay         time.Duration `yaml:"end_delay"`
	EndDisplayTime   time.Duration `yaml:"end_display_time"`
	ProgressDebounce time.Duration `yaml:"progress_debounce"`
}

// PaperlessConfig holds Paperless-ngx-specific settings.
type PaperlessConfig struct {
	Enabled      bool          `yaml:"enabled"`
	WebhookSecret string       `yaml:"webhook_secret"`
	Priority     int           `yaml:"priority"`
	CleanupDelay time.Duration `yaml:"cleanup_delay"`
	StaleTimeout time.Duration `yaml:"stale_timeout"`
	EndDelay     time.Duration `yaml:"end_delay"`
	EndDisplayTime time.Duration `yaml:"end_display_time"`
}

// ChangedetectionConfig holds Changedetection.io-specific settings.
type ChangedetectionConfig struct {
	Enabled      bool          `yaml:"enabled"`
	WebhookSecret string       `yaml:"webhook_secret"`
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
				ProgressDebounce: 30 * time.Second,
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
		},
	}

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config: %w", err)
		}
	}

	cfg.applyEnvOverrides()

	if cfg.Database.DSN == "" {
		return nil, fmt.Errorf("database.dsn is required (set PUSHWARD_DATABASE_DSN)")
	}

	return cfg, nil
}

func (cfg *Config) applyEnvOverrides() {
	cfg.Server.ApplyEnvOverrides()

	if v := os.Getenv("PUSHWARD_DATABASE_DSN"); v != "" {
		cfg.Database.DSN = v
	}
	if v := os.Getenv("PUSHWARD_DATABASE_PASSWORD_FILE"); v != "" {
		cfg.Database.PasswordFile = v
	}

	// Grafana overrides
	if v := os.Getenv("PUSHWARD_GRAFANA_WEBHOOK_SECRET"); v != "" {
		cfg.Providers.Grafana.WebhookSecret = v
	}
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
	if v := os.Getenv("PUSHWARD_ARGOCD_WEBHOOK_SECRET"); v != "" {
		cfg.Providers.ArgoCD.WebhookSecret = v
	}
	if v := os.Getenv("PUSHWARD_ARGOCD_URL"); v != "" {
		cfg.Providers.ArgoCD.URL = v
	}
	if v := os.Getenv("PUSHWARD_SYNC_GRACE_PERIOD"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Providers.ArgoCD.SyncGracePeriod = d
		}
	}

	// Jellyfin overrides
	if v := os.Getenv("PUSHWARD_JELLYFIN_WEBHOOK_SECRET"); v != "" {
		cfg.Providers.Jellyfin.WebhookSecret = v
	}

	// Paperless overrides
	if v := os.Getenv("PUSHWARD_PAPERLESS_WEBHOOK_SECRET"); v != "" {
		cfg.Providers.Paperless.WebhookSecret = v
	}

	// Changedetection overrides
	if v := os.Getenv("PUSHWARD_CHANGEDETECTION_WEBHOOK_SECRET"); v != "" {
		cfg.Providers.Changedetection.WebhookSecret = v
	}
}
