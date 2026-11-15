package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

func TestValidateWidgets_RejectsEmpty(t *testing.T) {
	cases := []struct {
		name    string
		input   WidgetConfig
		wantErr string
	}{
		{
			name:    "missing slug",
			input:   WidgetConfig{Query: "up"},
			wantErr: "slug is required",
		},
		{
			name:    "bad slug",
			input:   WidgetConfig{Slug: "BadSlug!", Query: "up"},
			wantErr: "slug must match",
		},
		{
			name:    "no query",
			input:   WidgetConfig{Slug: "x"},
			wantErr: "exactly one of `query` or `query_all`",
		},
		{
			name:    "both queries",
			input:   WidgetConfig{Slug: "x", Query: "a", QueryAll: "b", SlugTemplate: "x-{{.id}}"},
			wantErr: "exactly one of",
		},
		{
			name:    "multi without slug_template",
			input:   WidgetConfig{Slug: "x", QueryAll: "a"},
			wantErr: "slug_template",
		},
		{
			name:    "bad update_mode",
			input:   WidgetConfig{Slug: "x", Query: "up", UpdateMode: "weird"},
			wantErr: "update_mode",
		},
		{
			name:    "bad template",
			input:   WidgetConfig{Slug: "x", Query: "up", Template: "spaceship"},
			wantErr: "unknown template",
		},
		{
			name:    "progress without bounds",
			input:   WidgetConfig{Slug: "x", Query: "up", Template: "progress"},
			wantErr: "min_value and content.max_value",
		},
		{
			name:    "interval too short",
			input:   WidgetConfig{Slug: "x", Query: "up", Interval: time.Second},
			wantErr: "too short",
		},
		{
			name:    "trend with query_all",
			input:   WidgetConfig{Slug: "x", Template: "trend", QueryAll: "up", SlugTemplate: "x-{{.id}}"},
			wantErr: "template trend requires `query`",
		},
		{
			name:    "trend without query",
			input:   WidgetConfig{Slug: "x", Template: "trend"},
			wantErr: "template trend requires `query`",
		},
		{
			// The second operand of the trend guard: query IS set, so the
			// message has to stop claiming it is missing.
			name:    "trend with both queries",
			input:   WidgetConfig{Slug: "x", Template: "trend", Query: "up", QueryAll: "up", SlugTemplate: "x-{{.id}}"},
			wantErr: "template trend takes `query` only",
		},
		{
			name:    "gauge with an empty range",
			input:   WidgetConfig{Slug: "x", Query: "up", Template: "gauge", Content: WidgetContentConfig{MinValue: pushward.Float64Ptr(5), MaxValue: pushward.Float64Ptr(5)}},
			wantErr: "must be below content.max_value",
		},
		{
			name:    "tap action without a scheme",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{TapAction: &TapActionConfig{URL: "example.com/dash"}}},
			wantErr: "must be a valid URL with a scheme",
		},
		{
			name:    "tap action with a blocked scheme",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{TapAction: &TapActionConfig{URL: "javascript:alert(1)"}}},
			wantErr: "blocked scheme",
		},
		{
			name:    "http tap action without a host",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{TapAction: &TapActionConfig{URL: "https:///dash"}}},
			wantErr: "must include a host",
		},
		{
			name:    "webhook shape on a custom scheme",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{URLAction: &TapActionConfig{URL: "pushward://widgets", Method: "POST"}}},
			wantErr: "only valid for http(s) URLs",
		},
		{
			name:    "unsupported method",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{URLAction: &TapActionConfig{URL: "https://example.com", Method: "TRACE"}}},
			wantErr: "method must be one of",
		},
		{
			name:    "countdown with a query",
			input:   WidgetConfig{Slug: "x", Template: "countdown", Query: "up", Content: WidgetContentConfig{EndDate: soonRFC3339()}},
			wantErr: "template countdown is static",
		},
		{
			name:    "countdown without end_date",
			input:   WidgetConfig{Slug: "x", Template: "countdown"},
			wantErr: "requires content.end_date",
		},
		{
			name:    "stale_after below the floor",
			input:   WidgetConfig{Slug: "x", Query: "up", Interval: time.Minute, StaleAfter: pushward.IntPtr(90)},
			wantErr: "below 3 times",
		},
		{
			name:    "stale_after out of range",
			input:   WidgetConfig{Slug: "x", Query: "up", StaleAfter: pushward.IntPtr(10)},
			wantErr: "must be between 60 and 604800",
		},
		{
			name:    "stale_after on a countdown",
			input:   WidgetConfig{Slug: "x", Template: "countdown", StaleAfter: pushward.IntPtr(3600), Content: WidgetContentConfig{EndDate: soonRFC3339()}},
			wantErr: "has nothing to refresh",
		},
		{
			name:    "end_date not RFC 3339",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{EndDate: "tomorrow"}},
			wantErr: "is not an RFC 3339 timestamp",
		},
		{
			// The classic milliseconds-for-seconds mistake lands in 1970.
			name:    "end_date before the floor",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{EndDate: "1970-01-01T00:00:00Z"}},
			wantErr: "must fall between 2000-01-01",
		},
		{
			name:    "expired_text too long",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{ExpiredText: strings.Repeat("x", 65)}},
			wantErr: "expired_text exceeds 64",
		},
		{
			name:    "bad timer style",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{SubtitleTimer: &TimerConfig{Date: soonRFC3339(), Style: "sundial"}}},
			wantErr: "style must be one of timer, relative",
		},
		{
			name:    "timer without a date",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{SubtitleTimer: &TimerConfig{}}},
			wantErr: "date is required",
		},
		{
			name:    "bad trend",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{Trend: "sideways"}},
			wantErr: "trend must be one of",
		},
		{
			name:    "unknown style",
			input:   WidgetConfig{Slug: "x", Query: "up", Template: "status", Content: WidgetContentConfig{Style: "Retro"}},
			wantErr: `widgets[0] "x": content.style must be one of standard, bold, classic, got "retro"`,
		},
		{
			// a name from before the rename gets no alias outside the server
			name:    "pre-rename style",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{Style: "refined", StyleBySize: map[string]pushward.WidgetStyle{"small": "bold"}}},
			wantErr: `content.style must be one of standard, bold, classic, got "refined"`,
		},
		{
			name:    "style_by_size unknown size",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{StyleBySize: map[string]pushward.WidgetStyle{"xl": "bold"}}},
			wantErr: `content.style_by_size key must be one of small, medium, large, lock_screen, got "xl"`,
		},
		{
			name:    "style_by_size unknown style",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{StyleBySize: map[string]pushward.WidgetStyle{"Small": "loud"}}},
			wantErr: `content.style_by_size.small must be one of standard, bold, classic, got "loud"`,
		},
		{
			name:    "unknown density",
			input:   WidgetConfig{Slug: "s", Template: "stat_list", StatRows: []StatRowConfig{{Label: "L", Query: "up", ValueTemplate: "{{.Value}}"}}, Content: WidgetContentConfig{Density: "dense"}},
			wantErr: `content.density must be one of auto, compact, comfortable, got "dense"`,
		},
		{
			name:    "density_by_size unknown size",
			input:   WidgetConfig{Slug: "s", Template: "stat_list", StatRows: []StatRowConfig{{Label: "L", Query: "up", ValueTemplate: "{{.Value}}"}}, Content: WidgetContentConfig{DensityBySize: map[string]string{"watch": "auto"}}},
			wantErr: `content.density_by_size key must be one of small, medium, large, lock_screen, got "watch"`,
		},
		{
			name:    "density_by_size unknown density",
			input:   WidgetConfig{Slug: "s", Template: "stat_list", StatRows: []StatRowConfig{{Label: "L", Query: "up", ValueTemplate: "{{.Value}}"}}, Content: WidgetContentConfig{DensityBySize: map[string]string{"large": "tight"}}},
			wantErr: `content.density_by_size.large must be one of auto, compact, comfortable, got "tight"`,
		},
		{
			name:    "density off stat_list",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{Density: "compact"}},
			wantErr: `content.density is only available for template "stat_list", not "value"`,
		},
		{
			name:    "density_by_size off stat_list",
			input:   WidgetConfig{Slug: "x", Query: "up", Template: "status", Content: WidgetContentConfig{DensityBySize: map[string]string{"small": "compact"}}},
			wantErr: `content.density_by_size is only available for template "stat_list", not "status"`,
		},
		{
			name:    "tap action without a url",
			input:   WidgetConfig{Slug: "x", Query: "up", Content: WidgetContentConfig{URLAction: &TapActionConfig{Title: "Open"}}},
			wantErr: "url is required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateWidgets([]WidgetConfig{c.input})
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}

	// battery, schedule and flow stay rejected on purpose: each needs several
	// independent readings in one push, which one-query-per-widget cannot
	// express, and a spec with no source crashes the process at startup.
	for _, tmpl := range []string{"battery", "schedule", "flow"} {
		t.Run(tmpl+" still rejected", func(t *testing.T) {
			err := validateWidgets([]WidgetConfig{{Slug: "x", Query: "up", Template: tmpl}})
			if err == nil || !strings.Contains(err.Error(), "unknown template") {
				t.Fatalf("expected an unknown template error, got %v", err)
			}
		})
	}
}

func TestValidateWidgets_DuplicateSlug(t *testing.T) {
	err := validateWidgets([]WidgetConfig{
		{Slug: "x", Query: "up"},
		{Slug: "x", Query: "down"},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate slug") {
		t.Fatalf("expected duplicate slug error, got %v", err)
	}
}

func TestValidateWidgets_AppliesDefaults(t *testing.T) {
	cfgs := []WidgetConfig{{Slug: "users", Query: "up"}}
	if err := validateWidgets(cfgs); err != nil {
		t.Fatal(err)
	}
	w := cfgs[0]
	if w.Template != "value" {
		t.Errorf("Template default = %q, want value", w.Template)
	}
	if w.Interval != 60*time.Second {
		t.Errorf("Interval default = %v, want 60s", w.Interval)
	}
	if w.UpdateMode != "on_change" {
		t.Errorf("UpdateMode default = %q, want on_change", w.UpdateMode)
	}
	if w.Name != "users" {
		t.Errorf("Name default = %q, want %q", w.Name, w.Slug)
	}
}

func TestValidateWidgets_StatListAccepted(t *testing.T) {
	cfgs := []WidgetConfig{{
		Slug:     "stats",
		Template: "stat_list",
		StatRows: []StatRowConfig{
			{Label: "Users", Query: "users", ValueTemplate: "{{.Value}}"},
			{Label: "MRR", Query: "mrr", ValueTemplate: "{{.Value}}"},
		},
	}}
	if err := validateWidgets(cfgs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateWidgets_StatListRejectsCases(t *testing.T) {
	cases := []struct {
		name    string
		input   WidgetConfig
		wantErr string
	}{
		{
			"stat_list missing rows",
			WidgetConfig{Slug: "s", Template: "stat_list"},
			"requires `stat_rows`",
		},
		{
			"stat_list with query",
			WidgetConfig{Slug: "s", Template: "stat_list", Query: "x", StatRows: []StatRowConfig{{Label: "L", Query: "up", ValueTemplate: "{{.Value}}"}}},
			"must not set",
		},
		{
			"stat_rows on non-stat template",
			WidgetConfig{Slug: "s", Template: "value", Query: "up", StatRows: []StatRowConfig{{Label: "L", Query: "up", ValueTemplate: "{{.Value}}"}}},
			"stat_rows is only valid",
		},
		{
			"too many rows",
			WidgetConfig{Slug: "s", Template: "stat_list", StatRows: []StatRowConfig{
				{Label: "a", Query: "q", ValueTemplate: "{{.Value}}"},
				{Label: "b", Query: "q", ValueTemplate: "{{.Value}}"},
				{Label: "c", Query: "q", ValueTemplate: "{{.Value}}"},
				{Label: "d", Query: "q", ValueTemplate: "{{.Value}}"},
				{Label: "e", Query: "q", ValueTemplate: "{{.Value}}"},
				{Label: "f", Query: "q", ValueTemplate: "{{.Value}}"},
				{Label: "g", Query: "q", ValueTemplate: "{{.Value}}"},
			}},
			"exceeds server cap",
		},
		{
			"row missing query",
			WidgetConfig{Slug: "s", Template: "stat_list", StatRows: []StatRowConfig{{Label: "L", ValueTemplate: "{{.Value}}"}}},
			"query is required",
		},
		{
			"row missing template",
			WidgetConfig{Slug: "s", Template: "stat_list", StatRows: []StatRowConfig{{Label: "L", Query: "q"}}},
			"value_template is required",
		},
		{
			"row label too long",
			WidgetConfig{Slug: "s", Template: "stat_list", StatRows: []StatRowConfig{
				{Label: strings.Repeat("a", 33), Query: "q", ValueTemplate: "{{.Value}}"},
			}},
			"label exceeds",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateWidgets([]WidgetConfig{c.input})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want %q", err, c.wantErr)
			}
		})
	}
}

func TestParseWidgetsJSON_StatList(t *testing.T) {
	raw := `[
		{
			"slug": "pushward-stats",
			"name": "PushWard",
			"template": "stat_list",
			"interval": "60s",
			"update_mode": "on_change",
			"content": {"icon": "chart.bar.fill"},
			"stat_rows": [
				{"label": "Users", "query": "users_total", "value_template": "{{printf \"%.0f\" .Value}}"}
			]
		}
	]`
	widgets, err := parseWidgetsJSON(raw)
	if err != nil {
		t.Fatalf("parseWidgetsJSON: %v", err)
	}
	if len(widgets) != 1 {
		t.Fatalf("want 1 widget, got %d", len(widgets))
	}
	w := widgets[0]
	if w.Slug != "pushward-stats" || w.Template != "stat_list" {
		t.Errorf("decoded mismatch: %+v", w)
	}
	if w.Interval != 60*time.Second {
		t.Errorf("interval = %v, want 60s", w.Interval)
	}
	if len(w.StatRows) != 1 || w.StatRows[0].Label != "Users" {
		t.Errorf("stat_rows mismatch: %+v", w.StatRows)
	}
}

func TestParseWidgetsJSON_BadInterval(t *testing.T) {
	_, err := parseWidgetsJSON(`[{"slug":"x","name":"X","template":"value","query":"q","interval":"forever"}]`)
	if err == nil || !strings.Contains(err.Error(), "invalid interval") {
		t.Fatalf("want invalid interval error, got %v", err)
	}
}

func TestParseWidgetsJSON_UnknownField(t *testing.T) {
	_, err := parseWidgetsJSON(`[{"slug":"x","name":"X","bogus":"field"}]`)
	if err == nil {
		t.Fatal("want error on unknown field")
	}
}

func TestValidateWidgets_MultiAccepted(t *testing.T) {
	cfgs := []WidgetConfig{{
		Slug:         "group",
		QueryAll:     "up",
		SlugTemplate: "g-{{.instance}}",
	}}
	if err := validateWidgets(cfgs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateWidgets_StatListTrigger(t *testing.T) {
	base := func() WidgetConfig {
		return WidgetConfig{
			Slug:     "stats",
			Template: "stat_list",
			StatRows: []StatRowConfig{
				{Label: "Users", Query: "users", ValueTemplate: "{{.Value}}"},
				{Label: "Activities", Query: "act", ValueTemplate: "{{.Value}}", Trigger: pushward.BoolPtr(false)},
			},
		}
	}

	// One trigger row (Users defaults to true) + one display-only row is valid.
	if err := validateWidgets([]WidgetConfig{base()}); err != nil {
		t.Fatalf("mixed trigger config should be valid, got %v", err)
	}

	// All rows trigger:false under on_change is rejected.
	allOff := base()
	allOff.StatRows[0].Trigger = pushward.BoolPtr(false)
	err := validateWidgets([]WidgetConfig{allOff})
	if err == nil || !strings.Contains(err.Error(), "trigger:false") {
		t.Fatalf("want trigger:false rejection, got %v", err)
	}

	// The minimal misconfig: a lone display-only row under on_change.
	single := WidgetConfig{
		Slug:     "solo",
		Template: "stat_list",
		StatRows: []StatRowConfig{
			{Label: "Users", Query: "users", ValueTemplate: "{{.Value}}", Trigger: pushward.BoolPtr(false)},
		},
	}
	if err := validateWidgets([]WidgetConfig{single}); err == nil || !strings.Contains(err.Error(), "trigger:false") {
		t.Fatalf("single display-only row under on_change should be rejected, got %v", err)
	}

	// update_mode: always exempts the all-off card.
	allOff.UpdateMode = "always"
	if err := validateWidgets([]WidgetConfig{allOff}); err != nil {
		t.Fatalf("update_mode always should bypass the trigger check, got %v", err)
	}

	// Explicit trigger:true on every row is fine.
	allOn := base()
	allOn.StatRows[0].Trigger = pushward.BoolPtr(true)
	allOn.StatRows[1].Trigger = pushward.BoolPtr(true)
	if err := validateWidgets([]WidgetConfig{allOn}); err != nil {
		t.Fatalf("all-trigger config should be valid, got %v", err)
	}
}

func TestParseWidgetsJSON_Trigger(t *testing.T) {
	raw := `[
		{
			"slug": "pushward-stats",
			"template": "stat_list",
			"update_mode": "on_change",
			"stat_rows": [
				{"label": "Users", "query": "u", "value_template": "{{.Value}}"},
				{"label": "Activities", "query": "a", "value_template": "{{.Value}}", "trigger": false}
			]
		}
	]`
	widgets, err := parseWidgetsJSON(raw)
	if err != nil {
		t.Fatalf("parseWidgetsJSON: %v", err)
	}
	rows := widgets[0].StatRows
	if rows[0].Trigger != nil {
		t.Errorf("row 0 trigger = %v, want nil (defaulted)", rows[0].Trigger)
	}
	if rows[1].Trigger == nil || *rows[1].Trigger {
		t.Errorf("row 1 trigger = %v, want explicit false", rows[1].Trigger)
	}
}

func TestValidateWidgets_TrendAndCountdownAccepted(t *testing.T) {
	widgets := []WidgetConfig{
		{Slug: "cpu", Template: "trend", Query: "up", Interval: time.Minute, StaleAfter: pushward.IntPtr(600)},
		{Slug: "launch", Template: "countdown", Content: WidgetContentConfig{
			EndDate:     soonRFC3339(),
			ExpiredText: "Launched",
		}},
	}
	if err := validateWidgets(widgets); err != nil {
		t.Fatalf("expected trend and countdown to validate, got %v", err)
	}
	// The parsed date has to reach the wire content, not just pass validation.
	content := widgets[1].Content.ToWidgetContent()
	if content.EndDate == nil || !content.EndDate.After(time.Now()) {
		t.Errorf("expected a parsed future end_date on the widget content, got %v", content.EndDate)
	}
	if content.ExpiredText != "Launched" {
		t.Errorf("expected expired_text carried through, got %q", content.ExpiredText)
	}
}

func TestValidateWidgets_ContentExtrasReachTheWire(t *testing.T) {
	widgets := []WidgetConfig{{
		Slug: "x", Query: "up", Interval: time.Minute,
		Content: WidgetContentConfig{
			Trend:         "up",
			SubtitleTimer: &TimerConfig{Date: soonRFC3339(), Style: pushward.TimerStyleRelative},
			TapAction:     &TapActionConfig{URL: "pushward://widgets"},
			URLAction:     &TapActionConfig{URL: "https://grafana.example/d/abc", Foreground: true, Title: "Dashboard"},
		},
	}}
	if err := validateWidgets(widgets); err != nil {
		t.Fatalf("validateWidgets: %v", err)
	}
	c := widgets[0].Content.ToWidgetContent()
	if c.Trend != "up" {
		t.Errorf("expected trend up, got %q", c.Trend)
	}
	if c.SubtitleTimer == nil || c.SubtitleTimer.Style != pushward.TimerStyleRelative {
		t.Errorf("expected a relative subtitle timer, got %+v", c.SubtitleTimer)
	}
	if c.TapAction == nil || c.TapAction.URL != "pushward://widgets" {
		t.Errorf("expected the tap action carried through, got %+v", c.TapAction)
	}
	if c.URLAction == nil || c.URLAction.Title != "Dashboard" {
		t.Errorf("expected the url action carried through, got %+v", c.URLAction)
	}
}

// Unset looks must stay off the wire so the picks made in the app apply; set
// ones reach the content the manager sends on every create and PATCH, in the
// case they were written in (the server folds it).
func TestValidateWidgets_Looks(t *testing.T) {
	widgets := []WidgetConfig{
		{Slug: "plain", Query: "up"},
		{Slug: "gauge", Template: "gauge", Query: "up", Content: WidgetContentConfig{
			MinValue: pushward.Float64Ptr(0), MaxValue: pushward.Float64Ptr(1),
			Style: "classic", StyleBySize: map[string]pushward.WidgetStyle{"lock_screen": "bold"},
		}},
		{Slug: "launch", Template: "countdown", Content: WidgetContentConfig{EndDate: soonRFC3339(), Style: "Bold"}},
		{Slug: "rows", Template: "stat_list", StatRows: []StatRowConfig{{Label: "L", Query: "up", ValueTemplate: "{{.Value}}"}}, Content: WidgetContentConfig{
			Density: "comfortable", DensityBySize: map[string]string{"Small": "Compact"},
		}},
	}
	if err := validateWidgets(widgets); err != nil {
		t.Fatalf("validateWidgets: %v", err)
	}
	want := [][]string{
		nil,
		{`"style":"classic"`, `"style_by_size":{"lock_screen":"bold"}`},
		{`"style":"Bold"`},
		{`"density":"comfortable"`, `"density_by_size":{"Small":"Compact"}`},
	}
	for i, w := range widgets {
		b, err := json.Marshal(w.Content.ToWidgetContent())
		if err != nil {
			t.Fatal(err)
		}
		got := string(b)
		if want[i] == nil && (strings.Contains(got, `"style`) || strings.Contains(got, `"density`)) {
			t.Errorf("%s content = %s, want no style or density keys", w.Slug, got)
		}
		for _, key := range want[i] {
			if !strings.Contains(got, key) {
				t.Errorf("%s content = %s, want %s", w.Slug, got, key)
			}
		}
	}
}

// The YAML keys are what an operator writes, so Load has to take all four and
// stop at a value the server would refuse.
func TestLoad_WidgetLooks(t *testing.T) {
	clearDurationVars(t)
	t.Setenv("PUSHWARD_WIDGETS_JSON", "")
	t.Setenv("PUSHWARD_METRICS_URL", "http://victoria:8428")
	t.Setenv("PUSHWARD_URL", "https://api.pushward.app")
	t.Setenv("PUSHWARD_API_KEY", "hlk_test")
	load := func(yml string) (*Config, error) {
		path := filepath.Join(t.TempDir(), "config.yml")
		if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}

	cfg, err := load(`widgets:
  - slug: rows
    template: stat_list
    stat_rows:
      - {label: L, query: up, value_template: "{{.Value}}"}
    content:
      style: bold
      style_by_size: {lock_screen: classic}
      density: compact
      density_by_size: {large: comfortable}
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.Widgets[0].Content
	if c.Style != "bold" || c.StyleBySize["lock_screen"] != "classic" || c.Density != "compact" || c.DensityBySize["large"] != "comfortable" {
		t.Errorf("content = %+v, want the four looks from the file", c)
	}

	_, err = load(`widgets:
  - slug: up
    query: up
    content:
      density: compact
`)
	if err == nil || !strings.Contains(err.Error(), `content.density is only available for template "stat_list", not "value"`) {
		t.Errorf("err = %v, want density refused on value", err)
	}
}

// parseWidgetsJSON uses DisallowUnknownFields, so every new key needs a decode
// test or it stays a YAML-only field that silently 400s the env-var form.
func TestParseWidgetsJSON_ContentExtras(t *testing.T) {
	raw := `[{"slug":"x","query":"up","stale_after":600,"interval":"1m","content":{
		"start_date":"` + soonRFC3339() + `","end_date":"` + laterRFC3339() + `",
		"expired_text":"done","trend":"flat","style":"bold","style_by_size":{"small":"classic"},
		"subtitle_timer":{"date":"` + soonRFC3339() + `","style":"relative"},
		"tap_action":{"url":"pushward://widgets"},
		"url_action":{"url":"https://example.com","foreground":true,"title":"Open"},
		"secondary_url_action":{"url":"https://example.com/2","foreground":true}}},
		{"slug":"s","template":"stat_list","stat_rows":[
			{"label":"Next","query":"next","value_template":"{{.Value}}",
			 "timer":{"date":"` + laterRFC3339() + `","style":"relative"}}],
		 "content":{"density":"compact","density_by_size":{"lock_screen":"auto"}}}]`
	widgets, err := parseWidgetsJSON(raw)
	if err != nil {
		t.Fatalf("parseWidgetsJSON: %v", err)
	}
	if err := validateWidgets(widgets); err != nil {
		t.Fatalf("validateWidgets: %v", err)
	}
	if widgets[0].StaleAfter == nil || *widgets[0].StaleAfter != 600 {
		t.Errorf("expected stale_after 600, got %v", widgets[0].StaleAfter)
	}
	c := widgets[0].Content.ToWidgetContent()
	if c.StartDate == nil || c.EndDate == nil || c.SubtitleTimer == nil {
		t.Errorf("expected the parsed dates and timer on the content, got %+v", c)
	}
	if c.SecondaryURLAction == nil || c.SecondaryURLAction.URL != "https://example.com/2" {
		t.Errorf("expected the secondary url action, got %+v", c.SecondaryURLAction)
	}
	if c.Style != pushward.WidgetStyleBold || c.StyleBySize["small"] != pushward.WidgetStyleClassic {
		t.Errorf("expected style bold and classic on small, got %q %v", c.Style, c.StyleBySize)
	}
	if rows := widgets[1].Content.ToWidgetContent(); rows.Density != "compact" || rows.DensityBySize["lock_screen"] != "auto" {
		t.Errorf("expected density compact and auto on lock_screen, got %q %v", rows.Density, rows.DensityBySize)
	}
	// stat_rows[].timer rides a different parse path from content.subtitle_timer
	// and reaches the wire through StatListRow rather than WidgetContent.
	rowTimer := widgets[1].StatRows[0].ParsedTimer()
	if rowTimer == nil {
		t.Fatal("expected the stat row timer to be parsed")
	}
	if rowTimer.Style != pushward.TimerStyleRelative || rowTimer.Date.IsZero() {
		t.Errorf("stat row timer = %+v, want the relative style and a parsed date", rowTimer)
	}
}

// The server (and this validator) reject a date more than 366 days out, so the
// fixtures have to be relative rather than a fixed year.
func soonRFC3339() string  { return time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339) }
func laterRFC3339() string { return time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339) }
