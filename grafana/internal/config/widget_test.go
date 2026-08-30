package config

import (
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
		// battery, schedule and flow stay rejected on purpose: each needs several
		// independent readings in one push, which one-query-per-widget cannot
		// express, and a spec with no source crashes the process at startup.
		{
			name:    "battery still rejected",
			input:   WidgetConfig{Slug: "x", Query: "up", Template: "battery"},
			wantErr: "unknown template",
		},
		{
			name:    "schedule still rejected",
			input:   WidgetConfig{Slug: "x", Query: "up", Template: "schedule"},
			wantErr: "unknown template",
		},
		{
			name:    "flow still rejected",
			input:   WidgetConfig{Slug: "x", Query: "up", Template: "flow"},
			wantErr: "unknown template",
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

// parseWidgetsJSON uses DisallowUnknownFields, so every new key needs a decode
// test or it stays a YAML-only field that silently 400s the env-var form.
func TestParseWidgetsJSON_ContentExtras(t *testing.T) {
	raw := `[{"slug":"x","query":"up","stale_after":600,"interval":"1m","content":{
		"start_date":"` + soonRFC3339() + `","end_date":"` + laterRFC3339() + `",
		"expired_text":"done","trend":"flat",
		"subtitle_timer":{"date":"` + soonRFC3339() + `","style":"relative"},
		"tap_action":{"url":"pushward://widgets"},
		"url_action":{"url":"https://example.com","foreground":true,"title":"Open"},
		"secondary_url_action":{"url":"https://example.com/2","foreground":true}}}]`
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
}

// The server (and this validator) reject a date more than 366 days out, so the
// fixtures have to be relative rather than a fixed year.
func soonRFC3339() string  { return time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339) }
func laterRFC3339() string { return time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339) }
