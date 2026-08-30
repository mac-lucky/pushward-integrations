package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

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
	Template      string        `yaml:"template" json:"template"` // value|progress|status|gauge|stat_list|trend|countdown; default "value"
	Query         string        `yaml:"query" json:"query"`       // PromQL/MetricsQL - scalar variant
	QueryAll      string        `yaml:"query_all" json:"query_all"`
	Interval      time.Duration `yaml:"interval" json:"interval"`       // default 60s; clamped to >=5s
	UpdateMode    string        `yaml:"update_mode" json:"update_mode"` // "on_change" (default) | "always"
	MinChange     float64       `yaml:"min_change" json:"min_change"`   // change threshold; default 0 (any change)
	PushThrottle  *int          `yaml:"push_throttle" json:"push_throttle,omitempty"`
	LabelTemplate string        `yaml:"label_template" json:"label_template"`

	// StaleAfter is how many seconds after the last update iOS starts dimming
	// the widget as out of date (60-604800, nil for never). Setting it also
	// arms a heartbeat: the manager re-sends the stored content every
	// max(30s, stale_after/2), which the server records as a touch rather than
	// a push, so a flat metric cannot age the widget out.
	StaleAfter *int `yaml:"stale_after" json:"stale_after,omitempty"`
	// Multi-series-only fields:
	SlugTemplate   string `yaml:"slug_template" json:"slug_template"`     // e.g. "users-{{.instance}}"
	NameTemplate   string `yaml:"name_template" json:"name_template"`     // e.g. "Users on {{.instance}}"
	MaxSeries      int    `yaml:"max_series" json:"max_series"`           // per-spec cap; 0 -> shared default
	CleanupMissing bool   `yaml:"cleanup_missing" json:"cleanup_missing"` // DELETE widgets for series that disappear

	// StatRows is required when Template == "stat_list". Each row carries its
	// own PromQL query and a Go template that formats the polled value into
	// a display string (server stores Value as a pre-formatted string so the
	// integration owns rounding / currency / units).
	StatRows []StatRowConfig `yaml:"stat_rows" json:"stat_rows"`

	Content WidgetContentConfig `yaml:"content" json:"content"`
}

// StatRowConfig is one row of a stat_list widget. ValueTemplate is required
// - it controls how the polled float renders (e.g. `"$%.0f"`,
// `"{{printf \"%.1f\" .Value}}%"`). Vars: .Value (float64), .Unit (string).
// MissingValue is emitted when the query returns no data; defaults to the
// em dash in defaultMissingValue, which is exempt product typography.
type StatRowConfig struct {
	Label         string `yaml:"label" json:"label"`
	Query         string `yaml:"query" json:"query"`
	ValueTemplate string `yaml:"value_template" json:"value_template"`
	Unit          string `yaml:"unit" json:"unit"`
	MissingValue  string `yaml:"missing_value" json:"missing_value"`
	// Trigger controls whether a change in this row's value triggers a widget
	// update; defaults to true (nil -> true). Set it false to display the row
	// without letting its value drive PATCHes - useful when one row (e.g. a
	// user counter) should be the sole update trigger while volatile rows
	// (activity counts, DB size) ride along and refresh only when the trigger
	// row changes. With update_mode on_change, at least one row must remain a
	// trigger or the widget would never update after creation.
	Trigger *bool `yaml:"trigger" json:"trigger,omitempty"`

	// Timer renders the row's trailing text as a live timer on clients that
	// support it; ValueTemplate still supplies the static fallback.
	Timer *TimerConfig `yaml:"timer" json:"timer,omitempty"`
	timer *pushward.TimerValue
}

// TimerConfig is an RFC 3339 anchor plus a render style, for the widget
// subtitle slot and for stat_list rows.
type TimerConfig struct {
	Date  string `yaml:"date" json:"date"`
	Style string `yaml:"style" json:"style,omitempty"` // "timer" (default) | "relative"
}

// TapActionConfig mirrors pushward.TapAction. tap_action retargets the whole
// widget; url_action and secondary_url_action draw routed buttons.
type TapActionConfig struct {
	URL        string            `yaml:"url" json:"url"`
	Foreground bool              `yaml:"foreground" json:"foreground,omitempty"`
	Method     string            `yaml:"method" json:"method,omitempty"`
	Headers    map[string]string `yaml:"headers" json:"headers,omitempty"`
	Body       string            `yaml:"body" json:"body,omitempty"`
	Title      string            `yaml:"title" json:"title,omitempty"`
	Icon       string            `yaml:"icon" json:"icon,omitempty"`
}

func (t *TapActionConfig) toTapAction() *pushward.TapAction {
	if t == nil {
		return nil
	}
	return &pushward.TapAction{
		URL:        t.URL,
		Foreground: t.Foreground,
		Method:     t.Method,
		Headers:    t.Headers,
		Body:       t.Body,
		Title:      t.Title,
		Icon:       t.Icon,
	}
}

// Triggers reports whether a change in this row's value should drive a widget
// PATCH. Trigger is nil-defaulted to true; only an explicit false makes the
// row display-only (excluded from stat_list change detection).
func (r StatRowConfig) Triggers() bool { return r.Trigger == nil || *r.Trigger }

// ParsedTimer returns the row's timer once validateContent has parsed it.
func (r StatRowConfig) ParsedTimer() *pushward.TimerValue { return r.timer }

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

	// StartDate and EndDate are RFC 3339 timestamps driving the countdown
	// template (end_date required) and self-advancing progress bars.
	// ExpiredText replaces the counter once EndDate passes; without it the
	// client counts up from EndDate instead.
	StartDate   string `yaml:"start_date" json:"start_date,omitempty"`
	EndDate     string `yaml:"end_date" json:"end_date,omitempty"`
	ExpiredText string `yaml:"expired_text" json:"expired_text,omitempty"`

	// Trend is the directional arrow on value/gauge/trend widgets: up|down|flat.
	Trend string `yaml:"trend" json:"trend,omitempty"`

	// SubtitleTimer renders the subtitle as a live timer on any template.
	// Subtitle stays as the static fallback.
	SubtitleTimer *TimerConfig `yaml:"subtitle_timer" json:"subtitle_timer,omitempty"`

	// Tap-action routing. tap_action retargets the whole widget; the other two
	// draw buttons, which iOS renders on the Home Screen families only.
	TapAction          *TapActionConfig `yaml:"tap_action" json:"tap_action,omitempty"`
	URLAction          *TapActionConfig `yaml:"url_action" json:"url_action,omitempty"`
	SecondaryURLAction *TapActionConfig `yaml:"secondary_url_action" json:"secondary_url_action,omitempty"`

	// Parsed forms of the date and timer fields, populated by validateContent.
	startDate     *time.Time
	endDate       *time.Time
	subtitleTimer *pushward.TimerValue
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

		StartDate:     w.startDate,
		EndDate:       w.endDate,
		ExpiredText:   w.ExpiredText,
		Trend:         w.Trend,
		SubtitleTimer: w.subtitleTimer,

		TapAction:          w.TapAction.toTapAction(),
		URLAction:          w.URLAction.toTapAction(),
		SecondaryURLAction: w.SecondaryURLAction.toTapAction(),
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

// validateStatListTriggers rejects a stat_list under update_mode on_change
// where every row is trigger:false - such a widget would never PATCH after
// creation. update_mode always is exempt (it patches every tick regardless of
// which rows changed).
func validateStatListTriggers(idx int, w *WidgetConfig) error {
	if w.Template != "stat_list" || w.UpdateMode != "on_change" {
		return nil
	}
	for _, r := range w.StatRows {
		if r.Triggers() {
			return nil
		}
	}
	return fmt.Errorf("widgets[%d] %q: all stat_rows have trigger:false with update_mode on_change; the widget would never update -- keep a row as a trigger or set update_mode: always", idx, w.Slug)
}

// validWidgetTemplates lists the renderers this bridge can drive from PromQL.
// battery, schedule and flow are server templates deliberately not offered:
// each needs several independent readings in one push, and shared/widgets only
// knows how to carry one scalar, one label-keyed fan-out (which becomes one
// widget per series, not one widget with N elements) or a set of pre-formatted
// stat rows. Enabling one anyway is worse than useless - the spec builds, the
// create posts content with no devices, the server 422s, and Manager.Start
// propagating that takes the alert timelines down with it. Kept in sync with
// pushward-grafana-plugin/pkg/plugin/widgets/config.go, which runs this same
// manager off a Grafana datasource.
var validWidgetTemplates = map[string]bool{
	"value": true, "progress": true, "status": true, "gauge": true, "stat_list": true,
	"trend": true, "countdown": true,
}

// Server caps for the staleness window (pushward-server/internal/model/widget.go).
const (
	staleAfterMin = 60
	staleAfterMax = 604800

	// staleAfterIntervalRatio is how many poll intervals must fit inside a
	// staleness window. The heartbeat fires at half the window but rides the
	// poll ticker, so the worst-case gap between two updates is half the window
	// plus one interval. Requiring three intervals caps that at five sixths of
	// the window; the obvious-looking two would sit exactly on the boundary and
	// dim the widget once a cycle.
	staleAfterIntervalRatio = 3

	expiredTextMaxRune = 64
)

const widgetDateHorizon = 366 * 24 * time.Hour

var widgetDateFloor = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// validTimerStyles allows the empty string: the server reads it as the default
// "timer" style.
var validTimerStyles = map[string]bool{
	"": true, pushward.TimerStyleTimer: true, pushward.TimerStyleRelative: true,
}

// validTrends bounds content.trend, the directional arrow on value/gauge/trend
// widgets. It allows the empty string the way the server's own validWidgetTrends
// does, so the call site needs no unset guard. Deriving the arrow from the
// TrendSource buffer is the behaviour operators will eventually want, but
// auto-deriving silently would override an explicit setting; a trend: auto mode
// is the follow-on.
var validTrends = map[string]bool{
	"": true, pushward.TrendUp: true, pushward.TrendDown: true, pushward.TrendFlat: true,
}

// Server caps (mirror pushward-server/internal/model/widget.go). Validating
// here keeps misconfigurations on the integration side instead of bouncing
// off a 422 at runtime.
const (
	statListMaxRows      = 6
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
			return fmt.Errorf("widgets[%d] %q: unknown template %q (allowed: value|progress|status|gauge|stat_list|trend|countdown)", i, w.Slug, w.Template)
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
		switch w.Template {
		case "stat_list":
			if len(w.StatRows) == 0 {
				return fmt.Errorf("widgets[%d] %q: template stat_list requires `stat_rows` (1-%d rows)", i, w.Slug, statListMaxRows)
			}
			if w.Query != "" || w.QueryAll != "" {
				return fmt.Errorf("widgets[%d] %q: template stat_list must not set `query` or `query_all`; use per-row queries", i, w.Slug)
			}
		case "countdown":
			// A countdown renders from its own dates on device, so there is
			// nothing to poll and the widget is published once.
			if modes != 0 {
				return fmt.Errorf("widgets[%d] %q: template countdown is static; drop `query`, `query_all` and `stat_rows`", i, w.Slug)
			}
			if w.Content.EndDate == "" {
				return fmt.Errorf("widgets[%d] %q: template countdown requires content.end_date (RFC 3339)", i, w.Slug)
			}
		case "trend":
			// The sparkline comes from this bridge's own rolling buffer of one
			// query's readings, and there is one buffer per widget, so a
			// query_all fan-out has nowhere to keep per-series history.
			if w.Query == "" {
				return fmt.Errorf("widgets[%d] %q: template trend requires `query`", i, w.Slug)
			}
			if modes != 1 {
				return fmt.Errorf("widgets[%d] %q: template trend takes `query` only; `query_all` fan-out and `stat_rows` are not supported", i, w.Slug)
			}
		default:
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
		if err := validateStatListTriggers(i, w); err != nil {
			return err
		}
		if (w.Template == "progress" || w.Template == "gauge") && (w.Content.MinValue == nil || w.Content.MaxValue == nil) {
			return fmt.Errorf("widgets[%d] %q: template %q requires content.min_value and content.max_value", i, w.Slug, w.Template)
		}
		if err := validateStaleAfter(i, w, w.Interval); err != nil {
			return err
		}
		if err := w.validateContent(i); err != nil {
			return err
		}
	}
	return nil
}

// validateStaleAfter bounds the staleness window and rejects the two shapes
// that would make it misbehave rather than protect: a countdown, which is
// published once and has nothing to refresh, and a window so short relative to
// the poll interval that the widget reads stale between two healthy polls.
//
// interval is a parameter rather than read off w because the floor is a
// multiple of it: taking w.Interval here would make the check silently pass
// everything if it ever ran before the 60s default was applied.
func validateStaleAfter(idx int, w *WidgetConfig, interval time.Duration) error {
	if w.StaleAfter == nil {
		return nil
	}
	if w.Template == "countdown" {
		return fmt.Errorf("widgets[%d] %q: template countdown is published once and renders from its own dates, so stale_after has nothing to refresh", idx, w.Slug)
	}
	s := *w.StaleAfter
	if s < staleAfterMin || s > staleAfterMax {
		return fmt.Errorf("widgets[%d] %q: stale_after must be between %d and %d seconds, got %d", idx, w.Slug, staleAfterMin, staleAfterMax, s)
	}
	if floor := staleAfterIntervalRatio * int(interval/time.Second); s < floor {
		return fmt.Errorf("widgets[%d] %q: stale_after %ds is below %d times the %v poll interval (%ds); the heartbeat rides the poll ticker, so the widget would dim before the next refresh landed",
			idx, w.Slug, s, staleAfterIntervalRatio, interval, floor)
	}
	return nil
}

// validateContent parses the date and timer content fields and bounds them the
// way the server does, caching the parsed values for ToWidgetContent.
func (w *WidgetConfig) validateContent(idx int) error {
	c := &w.Content
	maxDate := time.Now().Add(widgetDateHorizon)
	fail := func(err error) error { return fmt.Errorf("widgets[%d] %q: %w", idx, w.Slug, err) }

	var err error
	if c.startDate, err = parseWidgetDate("content.start_date", c.StartDate, maxDate); err != nil {
		return fail(err)
	}
	if c.endDate, err = parseWidgetDate("content.end_date", c.EndDate, maxDate); err != nil {
		return fail(err)
	}
	if c.startDate != nil && c.endDate != nil && !c.startDate.Before(*c.endDate) {
		return fail(errors.New("content.start_date must be before content.end_date"))
	}
	if utf8.RuneCountInString(c.ExpiredText) > expiredTextMaxRune {
		return fail(fmt.Errorf("content.expired_text exceeds %d characters", expiredTextMaxRune))
	}
	if c.subtitleTimer, err = parseTimer("content.subtitle_timer", c.SubtitleTimer, maxDate); err != nil {
		return fail(err)
	}
	if !validTrends[c.Trend] {
		return fail(fmt.Errorf("content.trend must be one of up, down, flat (got %q)", c.Trend))
	}
	// The server rejects an empty or inverted range, and gauge/progress require
	// both ends, so a min == max slips past the required-pair check above.
	if c.MinValue != nil && c.MaxValue != nil && *c.MinValue >= *c.MaxValue {
		return fail(fmt.Errorf("content.min_value (%g) must be below content.max_value (%g)", *c.MinValue, *c.MaxValue))
	}
	for i := range w.StatRows {
		r := &w.StatRows[i]
		if r.timer, err = parseTimer(fmt.Sprintf("stat_rows[%d].timer", i), r.Timer, maxDate); err != nil {
			return fail(err)
		}
	}
	// The full server-side rules, not just "url is non-empty": for value,
	// status, countdown and stat_list the create is not deferred, so a widget
	// the server rejects fails inside Manager.Start and main.go exits 1 - a
	// typo'd dashboard URL would crash-loop the bridge and take the alert
	// timelines with it.
	if err := pushward.ValidateTapActionSlots(
		c.TapAction.toTapAction(),
		c.URLAction.toTapAction(),
		c.SecondaryURLAction.toTapAction(),
	); err != nil {
		return fail(fmt.Errorf("content.%w", err))
	}
	return nil
}

// parseWidgetDate parses an optional RFC 3339 timestamp within the server's
// accepted range. The past floor catches the classic milliseconds-for-seconds
// mistake, which lands in 1970.
func parseWidgetDate(field, raw string, maxDate time.Time) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %q is not an RFC 3339 timestamp (e.g. 2026-12-24T18:00:00Z)", field, raw)
	}
	if t.Before(widgetDateFloor) || t.After(maxDate) {
		return nil, fmt.Errorf("%s must fall between %s and %d days from now", field, widgetDateFloor.Format("2006-01-02"), int(widgetDateHorizon/(24*time.Hour)))
	}
	return &t, nil
}

func parseTimer(field string, tc *TimerConfig, maxDate time.Time) (*pushward.TimerValue, error) {
	if tc == nil {
		return nil, nil
	}
	if !validTimerStyles[tc.Style] {
		return nil, fmt.Errorf("%s.style must be one of timer, relative", field)
	}
	d, err := parseWidgetDate(field+".date", tc.Date, maxDate)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("%s.date is required", field)
	}
	return &pushward.TimerValue{Date: *d, Style: tc.Style}, nil
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
		// Not sharedconfig.DefaultPushWardConfig(): this bridge diverges on three
		// of its five fields. Priority 5 outranks a build or a download, an alert
		// stays firing until something clears it so the TTL is a day rather than
		// half an hour, and EndDelay/EndDisplayTime stay unset because alerts end
		// in one shot instead of the two-phase close the pollers use.
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
	if err := sharedconfig.EnvDuration("PUSHWARD_METRICS_TIMEOUT", &cfg.Metrics.Timeout); err != nil {
		return err
	}
	if v := os.Getenv("PUSHWARD_GRAFANA_URL"); v != "" {
		cfg.Grafana.URL = v
	}
	if v := os.Getenv("PUSHWARD_GRAFANA_API_TOKEN"); v != "" {
		cfg.Grafana.APIToken = v
	}
	if err := sharedconfig.EnvDuration("PUSHWARD_ALERT_CHECK_INTERVAL", &cfg.Grafana.AlertCheckInterval); err != nil {
		return err
	}
	if err := sharedconfig.EnvDuration("PUSHWARD_HISTORY_WINDOW", &cfg.Timeline.HistoryWindow); err != nil {
		return err
	}
	if err := sharedconfig.EnvDuration("PUSHWARD_POLL_INTERVAL", &cfg.Timeline.PollInterval); err != nil {
		return err
	}
	if v := os.Getenv("PUSHWARD_WEBHOOK_TOKEN"); v != "" {
		cfg.WebhookToken = v
	}
	if v := os.Getenv("PUSHWARD_WIDGETS_JSON"); v != "" {
		// Replaces the YAML widgets list wholesale - we don't merge because
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
// helm values stay legible - time.Duration's default JSON encoding is integer
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
