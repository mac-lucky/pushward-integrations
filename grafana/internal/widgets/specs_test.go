package widgets

import (
	"strings"
	"testing"
	"time"

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
	// All rows default to trigger:true -> nil mask (fast path preserved).
	if specs[1].StatChangeMask != nil {
		t.Errorf("all-default widget mask = %v, want nil", specs[1].StatChangeMask)
	}
}

func TestBuildSpecsAttachesTrendAndCountdownSources(t *testing.T) {
	specs, err := BuildSpecs([]config.WidgetConfig{
		{Slug: "cpu", Template: "trend", Query: "up", Interval: time.Minute, StaleAfter: pushward.IntPtr(600)},
		{Slug: "launch", Template: "countdown"},
	}, nil)
	if err != nil {
		t.Fatalf("BuildSpecs: %v", err)
	}
	if _, ok := specs[0].Source.(*TrendSource); !ok {
		t.Errorf("expected a TrendSource for the trend template, got %T", specs[0].Source)
	}
	if _, ok := specs[1].Source.(staticSource); !ok {
		t.Errorf("expected a staticSource for the countdown template, got %T", specs[1].Source)
	}
	if specs[0].StaleAfter == nil || *specs[0].StaleAfter != 600 {
		t.Errorf("expected stale_after carried into the spec, got %v", specs[0].StaleAfter)
	}
	// The heartbeat is what keeps a flat metric from ageing the widget out.
	if specs[0].Heartbeat == 0 {
		t.Error("expected a heartbeat alongside stale_after")
	}
	if specs[1].StaleAfter != nil {
		t.Error("expected no stale_after on the countdown")
	}
}

// The defensive gate is what stops a hand-widened template allowlist from
// becoming a startup crash: a spec with no source posts content the server
// 422s, and Manager.Start propagating that exits the process.
func TestBuildSpecsRejectsAnUnsourceableTemplate(t *testing.T) {
	_, err := BuildSpecs([]config.WidgetConfig{{Slug: "batt", Template: "battery"}}, nil)
	if err == nil {
		t.Fatal("expected an error for a template with no source")
	}
	if !strings.Contains(err.Error(), "has no source") {
		t.Errorf("unexpected error: %v", err)
	}
}
