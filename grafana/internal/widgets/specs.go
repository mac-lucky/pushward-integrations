package widgets

import (
	"fmt"
	"slices"

	"github.com/mac-lucky/pushward-integrations/grafana/internal/config"
	"github.com/mac-lucky/pushward-integrations/grafana/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	sharedwidgets "github.com/mac-lucky/pushward-integrations/shared/widgets"
)

// BuildSpecs converts the grafana config into shared widget specs, attaching
// the appropriate Prometheus source (scalar, multi-series, or stat_list) for
// each widget. Returns an error if a stat_list source fails to compile (bad
// value template); other modes can't fail at build time.
func BuildSpecs(cfgs []config.WidgetConfig, mc *metrics.Client) ([]sharedwidgets.Spec, error) {
	specs := make([]sharedwidgets.Spec, 0, len(cfgs))
	for _, w := range cfgs {
		spec := sharedwidgets.Spec{
			Slug:           w.Slug,
			Name:           w.Name,
			Template:       pushward.WidgetTemplate(w.Template),
			Interval:       w.Interval,
			UpdateMode:     sharedwidgets.UpdateMode(w.UpdateMode),
			MinChange:      w.MinChange,
			PushThrottle:   w.PushThrottle,
			Content:        w.Content.ToWidgetContent(),
			StaleAfter:     w.StaleAfter,
			Heartbeat:      sharedwidgets.HeartbeatFor(w.StaleAfter),
			LabelTemplate:  w.LabelTemplate,
			SlugTemplate:   w.SlugTemplate,
			NameTemplate:   w.NameTemplate,
			MaxSeries:      w.MaxSeries,
			CleanupMissing: w.CleanupMissing,
		}
		switch {
		case w.Template == string(pushward.WidgetTemplateStatList):
			rows := make([]StatListRow, 0, len(w.StatRows))
			mask := make([]bool, len(w.StatRows))
			for i, r := range w.StatRows {
				rows = append(rows, StatListRow{
					Label: r.Label, Query: r.Query, ValueTemplate: r.ValueTemplate,
					Unit: r.Unit, MissingValue: r.MissingValue, Timer: r.ParsedTimer(),
				})
				mask[i] = r.Triggers()
			}
			src, err := NewStatListSource(mc, rows)
			if err != nil {
				return nil, fmt.Errorf("widget %q: %w", w.Slug, err)
			}
			spec.StatListSource = src
			// Attach the mask only when a row opted out; an all-true mask is
			// behaviorally identical to nil, which keeps the fast path.
			if slices.Contains(mask, false) {
				spec.StatChangeMask = mask
			}
		case w.Template == string(pushward.WidgetTemplateTrend):
			spec.Source = NewTrendSource(mc, w.Query, w.Interval)
		case w.Template == string(pushward.WidgetTemplateCountdown):
			spec.Source = staticSource{}
		case w.Query != "":
			spec.Source = &ScalarSource{Client: mc, Expr: w.Query}
		case w.QueryAll != "":
			spec.MultiSource = &MultiSource{Client: mc, Expr: w.QueryAll}
		default:
			// Second gate behind config.validWidgetTemplates. A template with no
			// source builds cleanly, posts content the server 422s, and takes the
			// whole process down through Manager.Start; refusing here keeps a
			// hand-widened allowlist from turning into a startup crash.
			return nil, fmt.Errorf("widget %q: template %q has no source in this bridge", w.Slug, w.Template)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}
