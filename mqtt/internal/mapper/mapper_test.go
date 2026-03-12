package mapper

import (
	"testing"

	"github.com/mac-lucky/pushward-integrations/mqtt/internal/config"
)

func TestMap_BasicFields(t *testing.T) {
	data := map[string]any{
		"running_state": "running",
		"program_name":  "Cotton 60",
	}

	mapping := config.ContentMapping{
		State:    "{running_state}",
		Subtitle: "{program_name}",
	}

	c := Map(data, mapping, "generic")

	if c.Template != "generic" {
		t.Errorf("Template = %q, want generic", c.Template)
	}
	if c.State != "running" {
		t.Errorf("State = %q, want running", c.State)
	}
	if c.Subtitle != "Cotton 60" {
		t.Errorf("Subtitle = %q, want Cotton 60", c.Subtitle)
	}
}

func TestMap_FieldOrMap_Default(t *testing.T) {
	data := map[string]any{
		"running_state": "unknown_state",
	}

	mapping := config.ContentMapping{
		Icon: config.FieldOrMap{
			Default: "washer.fill",
			Map: map[string]map[string]string{
				"running_state": {
					"paused": "pause.circle.fill",
				},
			},
		},
	}

	c := Map(data, mapping, "generic")
	if c.Icon != "washer.fill" {
		t.Errorf("Icon = %q, want washer.fill", c.Icon)
	}
}

func TestMap_FieldOrMap_Mapped(t *testing.T) {
	data := map[string]any{
		"running_state": "paused",
	}

	mapping := config.ContentMapping{
		Icon: config.FieldOrMap{
			Default: "washer.fill",
			Map: map[string]map[string]string{
				"running_state": {
					"paused": "pause.circle.fill",
				},
			},
		},
	}

	c := Map(data, mapping, "generic")
	if c.Icon != "pause.circle.fill" {
		t.Errorf("Icon = %q, want pause.circle.fill", c.Icon)
	}
}

func TestMap_Progress_Float(t *testing.T) {
	data := map[string]any{
		"completion_percentage": float64(75),
	}

	mapping := config.ContentMapping{
		Progress: "{completion_percentage | div:100}",
	}

	c := Map(data, mapping, "generic")
	if c.Progress != 0.75 {
		t.Errorf("Progress = %v, want 0.75", c.Progress)
	}
}

func TestMap_RemainingTime_Int(t *testing.T) {
	data := map[string]any{
		"remaining_minutes": float64(30),
	}

	mapping := config.ContentMapping{
		RemainingTime: "{remaining_minutes | mul:60}",
	}

	c := Map(data, mapping, "generic")
	if c.RemainingTime == nil || *c.RemainingTime != 1800 {
		t.Errorf("RemainingTime = %v, want 1800", c.RemainingTime)
	}
}

func TestMap_AccentColor_Mapped(t *testing.T) {
	data := map[string]any{
		"running_state": "done",
	}

	mapping := config.ContentMapping{
		AccentColor: config.FieldOrMap{
			Default: "blue",
			Map: map[string]map[string]string{
				"running_state": {
					"paused": "orange",
					"done":   "green",
				},
			},
		},
	}

	c := Map(data, mapping, "generic")
	if c.AccentColor != "green" {
		t.Errorf("AccentColor = %q, want green", c.AccentColor)
	}
}
