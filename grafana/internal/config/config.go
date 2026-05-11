package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// Config is the top-level configuration for pushward-grafana.
type Config struct {
	Server       sharedconfig.ServerConfig   `yaml:"server"`
	PushWard     sharedconfig.PushWardConfig `yaml:"pushward"`
	Metrics      MetricsConfig               `yaml:"metrics"`
	Grafana      GrafanaConfig               `yaml:"grafana"`
	Timeline     TimelineConfig              `yaml:"timeline"`
	Widgets      []WidgetConfig              `yaml:"widgets"`
	WebhookToken string                      `yaml:"webhook_token"`
}

// WidgetConfig declares one widget the integration polls and publishes via
// the pushward-server widget API. Exactly one of Query (scalar) or QueryAll
// (multi-series fan-out) must be set.
//
// The API key in cfg.PushWard.APIKey must be an integration key (hlk_) with
// the `widgets` scope enabled; without it the server returns 403 on
// CreateWidget at startup.
type WidgetConfig struct {
	Slug          string        `yaml:"slug" json:"slug"`
	Name          string        `yaml:"name" json:"name"`
	Template      string        `yaml:"template" json:"template"` // value|progress|status|gauge|stat_list; default "value"
	Query         string        `yaml:"query" json:"query"`       // PromQL/MetricsQL — scalar variant
	QueryAll      string        `yaml:"query_all" json:"query_all"`
	Interval      time.Duration `yaml:"interval" json:"interval"`       // default 60s; clamped to ≥5s
	UpdateMode    string        `yaml:"update_mode" json:"update_mode"` // "on_change" (default) | "always"
	MinChange     float64       `yaml:"min_change" json:"min_change"`   // change threshold; default 0 (any change)
	PushThrottle  *int          `yaml:"push_throttle" json:"push_throttle,omitempty"`
	LabelTemplate string        `yaml:"label_template" json:"label_template"`
	// Multi-series-only fields:
	SlugTemplate   string `yaml:"slug_template" json:"slug_template"`     // e.g. "users-{{.instance}}"
	NameTemplate   string `yaml:"name_template" json:"name_template"`     // e.g. "Users on {{.instance}}"
	MaxSeries      int    `yaml:"max_series" json:"max_series"`           // per-spec cap; 0 → shared default
	CleanupMissing bool   `yaml:"cleanup_missing" json:"cleanup_missing"` // DELETE widgets for series that disappear

	// StatRows is required when Template == "stat_list". Each row carries its
	// own PromQL query and a Go template that formats the polled value into
	// a display string (server stores Value as a pre-formatted string so the
	// integration owns rounding / currency / units).
	StatRows []StatRowConfig `yaml:"stat_rows" json:"stat_rows"`

	Content WidgetContentConfig `yaml:"content" json:"content"`
}

// StatRowConfig is one row of a stat_list widget. ValueTemplate is required
// — it controls how the polled float renders (e.g. `"$%.0f"`,
// `"{{printf \"%.1f\" .Value}}%"`). Vars: .Value (float64), .Unit (string).
// MissingValue is emitted when the query returns no data; defaults to "—".
type StatRowConfig struct {
	Label         string `yaml:"label" json:"label"`
	Query         string `yaml:"query" json:"query"`
	ValueTemplate string `yaml:"value_template" json:"value_template"`
	Unit          string `yaml:"unit" json:"unit"`
	MissingValue  string `yaml:"missing_value" json:"missing_value"`
}

// WidgetContentConfig is the static portion of pushward.WidgetContent
// supplied via YAML. The Value field is populated per-tick from the query.
type WidgetContentConfig struct {
	Icon            string   `yaml:"icon" json:"icon"`
	Unit            string   `yaml:"unit" json:"unit"`
	Subtitle        string   `yaml:"subtitle" json:"subtitle"`
	Severity        string   `yaml:"severity" json:"severity"`
	MinValue        *float64 `yaml:"min_value" json:"min_value,omitempty"`
	MaxValue        *float64 `yaml:"max_value" json:"max_value,omitempty"`
	AccentColor     string   `yaml:"accent_color" json:"accent_color"`
	BackgroundColor string   `yaml:"background_color" json:"background_color"`
	TextColor       string   `yaml:"text_color" json:"text_color"`
}

// ToWidgetContent maps the YAML-friendly config shape to the typed pushward
// content struct. Value is intentionally left unset; the manager fills it.
func (w WidgetContentConfig) ToWidgetContent() pushward.WidgetContent {
	return pushward.WidgetContent{
		Icon:            w.Icon,
		MinValue:        w.MinValue,
		MaxValue:        w.MaxValue,
		Unit:            w.Unit,
		Subtitle:        w.Subtitle,
		Severity:        w.Severity,
		AccentColor:     w.AccentColor,
		BackgroundColor: w.BackgroundColor,
		TextColor:       w.TextColor,
	}
}

var widgetSlugRE = regexp.MustCompile(`^[a-z0-9_-]{1,128}$`)

// validateStatRows enforces the server's row-count cap and per-field length
// limits at config load so misconfigurations don't make it to a runtime 422.
func validateStatRows(slug string, idx int, rows []StatRowConfig) error {
	if len(rows) == 0 {
		return nil
	}
	if len(rows) > statListMaxRows {
		return fmt.Errorf("widgets[%d] %q: stat_rows exceeds server cap (%d max, got %d)", idx, slug, statListMaxRows, len(rows))
	}
	for j, row := range rows {
		if row.Label == "" {
			return fmt.Errorf("widgets[%d] %q: stat_rows[%d].label is required", idx, slug, j)
		}
		if row.Query == "" {
			return fmt.Errorf("widgets[%d] %q: stat_rows[%d].query is required", idx, slug, j)
		}
		if row.ValueTemplate == "" {
			return fmt.Errorf("widgets[%d] %q: stat_rows[%d].value_template is required", idx, slug, j)
		}
		if runeLen(row.Label) > statListLabelMaxRune {
			return fmt.Errorf("widgets[%d] %q: stat_rows[%d].label exceeds %d characters", idx, slug, j, statListLabelMaxRune)
		}
		if runeLen(row.Unit) > statListUnitMaxRune {
			return fmt.Errorf("widgets[%d] %q: stat_rows[%d].unit exceeds %d characters", idx, slug, j, statListUnitMaxRune)
		}
	}
	return nil
}

func runeLen(s string) int { return len([]rune(s)) }

// validWidgetTemplates lists the renderers supported by the server today.
// Keep this in sync with pushward-server's internal/model/widget.go.
var validWidgetTemplates = map[string]bool{
	"value": true, "progress": true, "status": true, "gauge": true, "stat_list": true,
}

// Server caps (mirror pushward-server/internal/model/widget.go). Validating
// here keeps misconfigurations on the integration side instead of bouncing
// off a 422 at runtime.
const (
	statListMaxRows      = 4
	statListLabelMaxRune = 32
	statListUnitMaxRune  = 16
)

// validateWidgets normalises defaults and rejects malformed widget configs.
func validateWidgets(widgets []WidgetConfig) error {
	seen := make(map[string]int, len(widgets))
	for i := range widgets {
		w := &widgets[i]
		if w.Slug == "" {
			return fmt.Errorf("widgets[%d]: slug is required", i)
		}
		if !widgetSlugRE.MatchString(w.Slug) {
			return fmt.Errorf("widgets[%d] %q: slug must match %s", i, w.Slug, widgetSlugRE)
		}
		if prev, ok := seen[w.Slug]; ok {
			return fmt.Errorf("widgets[%d] %q: duplicate slug (already used by widgets[%d])", i, w.Slug, prev)
		}
		seen[w.Slug] = i
		if w.Name == "" {
			w.Name = w.Slug
		}
		if w.Template == "" {
			w.Template = "value"
		}
		if !validWidgetTemplates[w.Template] {
			return fmt.Errorf("widgets[%d] %q: unknown template %q (allowed: value|progress|status|gauge|stat_list)", i, w.Slug, w.Template)
		}
		modes := 0
		if w.Query != "" {
			modes++
		}
		if w.QueryAll != "" {
			modes++
		}
		if len(w.StatRows) > 0 {
			modes++
		}
		if w.Template == "stat_list" {
			if len(w.StatRows) == 0 {
				return fmt.Errorf("widgets[%d] %q: template stat_list requires `stat_rows` (1-%d rows)", i, w.Slug, statListMaxRows)
			}
			if w.Query != "" || w.QueryAll != "" {
				return fmt.Errorf("widgets[%d] %q: template stat_list must not set `query` or `query_all`; use per-row queries", i, w.Slug)
			}
		} else {
			if modes != 1 || len(w.StatRows) > 0 {
				return fmt.Errorf("widgets[%d] %q: exactly one of `query` or `query_all` must be set (stat_rows is only valid with template stat_list)", i, w.Slug)
			}
			if w.QueryAll != "" && w.SlugTemplate == "" {
				return fmt.Errorf("widgets[%d] %q: `slug_template` is required when `query_all` is set", i, w.Slug)
			}
		}
		if err := validateStatRows(w.Slug, i, w.StatRows); err != nil {
			return err
		}
		if w.Interval == 0 {
			w.Interval = 60 * time.Second
		}
		if w.Interval < 5*time.Second {
			return fmt.Errorf("widgets[%d] %q: interval %v is too short; minimum is 5s", i, w.Slug, w.Interval)
		}
		if w.UpdateMode == "" {
			w.UpdateMode = "on_change"
		}
		if w.UpdateMode != "on_change" && w.UpdateMode != "always" {
			return fmt.Errorf("widgets[%d] %q: unknown update_mode %q (allowed: on_change|always)", i, w.Slug, w.UpdateMode)
		}
		if (w.Template == "progress" || w.Template == "gauge") && (w.Content.MinValue == nil || w.Content.MaxValue == nil) {
			return fmt.Errorf("widgets[%d] %q: template %q requires content.min_value and content.max_value", i, w.Slug, w.Template)
		}
	}
	return nil
}

// MetricsConfig holds the Prometheus/VictoriaMetrics connection details.
type MetricsConfig struct {
	URL         string        `yaml:"url"`
	Username    string        `yaml:"username"`
	Password    string        `yaml:"password"`
	BearerToken string        `yaml:"bearer_token"`
	Timeout     time.Duration `yaml:"timeout"`
}

// GrafanaConfig holds optional Grafana API connection for auto-extracting queries.
type GrafanaConfig struct {
	URL                string        `yaml:"url"`
	APIToken           string        `yaml:"api_token"` // Editor-role service account token
	AlertCheckInterval time.Duration `yaml:"alert_check_interval"`
}

// TimelineConfig embeds shared visual settings and adds Grafana-specific fields.
type TimelineConfig struct {
	sharedconfig.TimelineConfig `yaml:",inline"`
	HistoryWindow               time.Duration `yaml:"history_window"`
	PollInterval                time.Duration `yaml:"poll_interval"`
	SeverityLabel               string        `yaml:"severity_label"`
	DefaultSeverity             string        `yaml:"default_severity"`
}

// Load reads the config file and applies environment variable overrides.
func Load(path string) (*Config, error) {
	smoothing := true
	decimals := 1
	cfg := &Config{
		Server: sharedconfig.ServerConfig{
			Address: ":8090",
		},
		PushWard: sharedconfig.PushWardConfig{
			Priority:     5,
			CleanupDelay: 15 * time.Minute,
			StaleTimeout: 24 * time.Hour,
		},
		Timeline: TimelineConfig{
			TimelineConfig: sharedconfig.TimelineConfig{
				Smoothing: &smoothing,
				Scale:     "linear",
				Decimals:  &decimals,
			},
			HistoryWindow:   30 * time.Minute,
			PollInterval:    30 * time.Second,
			SeverityLabel:   "severity",
			DefaultSeverity: "warning",
		},
	}

	if err := sharedconfig.LoadYAML(path, cfg); err != nil {
		return nil, err
	}

	cfg.Server.ApplyEnvOverrides()
	if err := applyEnvOverrides(cfg); err != nil {
		return nil, err
	}

	if err := cfg.PushWard.ApplyEnvOverrides(); err != nil {
		return nil, err
	}
	if err := cfg.PushWard.Validate(); err != nil {
		return nil, err
	}

	if cfg.Metrics.URL == "" {
		return nil, fmt.Errorf("metrics.url is required (set PUSHWARD_METRICS_URL)")
	}

	if err := validateWidgets(cfg.Widgets); err != nil {
		return nil, err
	}

	return cfg, nil
}

// AutoExtractEnabled reports whether the Grafana API is configured for query auto-extraction.
func (c *Config) AutoExtractEnabled() bool {
	return c.Grafana.URL != "" && c.Grafana.APIToken != ""
}

func applyEnvOverrides(cfg *Config) error {
	if v := os.Getenv("PUSHWARD_METRICS_URL"); v != "" {
		cfg.Metrics.URL = v
	}
	if v := os.Getenv("PUSHWARD_METRICS_USERNAME"); v != "" {
		cfg.Metrics.Username = v
	}
	if v := os.Getenv("PUSHWARD_METRICS_PASSWORD"); v != "" {
		cfg.Metrics.Password = v
	}
	if v := os.Getenv("PUSHWARD_METRICS_BEARER_TOKEN"); v != "" {
		cfg.Metrics.BearerToken = v
	}
	if v := os.Getenv("PUSHWARD_METRICS_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid PUSHWARD_METRICS_TIMEOUT %q: %w", v, err)
		}
		cfg.Metrics.Timeout = d
	}
	if v := os.Getenv("PUSHWARD_GRAFANA_URL"); v != "" {
		cfg.Grafana.URL = v
	}
	if v := os.Getenv("PUSHWARD_GRAFANA_API_TOKEN"); v != "" {
		cfg.Grafana.APIToken = v
	}
	if v := os.Getenv("PUSHWARD_ALERT_CHECK_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid PUSHWARD_ALERT_CHECK_INTERVAL %q: %w", v, err)
		}
		cfg.Grafana.AlertCheckInterval = d
	}
	if v := os.Getenv("PUSHWARD_HISTORY_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid PUSHWARD_HISTORY_WINDOW %q: %w", v, err)
		}
		cfg.Timeline.HistoryWindow = d
	}
	if v := os.Getenv("PUSHWARD_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid PUSHWARD_POLL_INTERVAL %q: %w", v, err)
		}
		cfg.Timeline.PollInterval = d
	}
	if v := os.Getenv("PUSHWARD_WEBHOOK_TOKEN"); v != "" {
		cfg.WebhookToken = v
	}
	if v := os.Getenv("PUSHWARD_WIDGETS_JSON"); v != "" {
		// Replaces the YAML widgets list wholesale — we don't merge because
		// there's no stable key to merge by (slugs aren't unique across the
		// two sources by contract). Helm charts pass the full list via env.
		widgets, err := parseWidgetsJSON(v)
		if err != nil {
			return fmt.Errorf("invalid PUSHWARD_WIDGETS_JSON: %w", err)
		}
		cfg.Widgets = widgets
	}
	return nil
}

// parseWidgetsJSON decodes the env-var JSON payload into []WidgetConfig.
// Interval is read as a Go duration string ("60s") rather than nanoseconds so
// helm values stay legible — time.Duration's default JSON encoding is integer
// nanoseconds, which is awful for humans editing values.yaml.
func parseWidgetsJSON(raw string) ([]WidgetConfig, error) {
	type widgetIn struct {
		WidgetConfig
		Interval string `json:"interval"`
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var in []widgetIn
	if err := dec.Decode(&in); err != nil {
		return nil, err
	}
	out := make([]WidgetConfig, len(in))
	for i, w := range in {
		out[i] = w.WidgetConfig
		if w.Interval != "" {
			d, err := time.ParseDuration(w.Interval)
			if err != nil {
				return nil, fmt.Errorf("widgets[%d] %q: invalid interval %q: %w", i, w.Slug, w.Interval, err)
			}
			out[i].Interval = d
		}
	}
	return out, nil
}
