package widgets

import (
	"testing"

	"github.com/mac-lucky/pushward-integrations/grafana/internal/config"
	"github.com/mac-lucky/pushward-integrations/grafana/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

func TestBuildSpecs_StatListChangeMask(t *testing.T) {
	mc := metrics.NewClient("http://example.invalid")

	cfgs := []config.WidgetConfig{
		{
			Slug:     "with-display-only",
			Template: "stat_list",
			StatRows: []config.StatRowConfig{
				{Label: "Users", Query: "u", ValueTemplate: "{{.Value}}"},
				{Label: "Activities", Query: "a", ValueTemplate: "{{.Value}}", Trigger: pushward.BoolPtr(false)},
			},
		},
		{
			Slug:     "all-default",
			Template: "stat_list",
			StatRows: []config.StatRowConfig{
				{Label: "Users", Query: "u", ValueTemplate: "{{.Value}}"},
				{Label: "MRR", Query: "m", ValueTemplate: "{{.Value}}"},
			},
		},
	}

	specs, err := BuildSpecs(cfgs, mc)
	if err != nil {
		t.Fatalf("BuildSpecs: %v", err)
	}

	// A display-only row produces a mask aligned to row order.
	if got := specs[0].StatChangeMask; len(got) != 2 || !got[0] || got[1] {
		t.Errorf("mask = %v, want [true false]", got)
	}
	// All rows default to trigger:true → nil mask (fast path preserved).
	if specs[1].StatChangeMask != nil {
		t.Errorf("all-default widget mask = %v, want nil", specs[1].StatChangeMask)
	}
}
