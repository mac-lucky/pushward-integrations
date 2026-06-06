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
