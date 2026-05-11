package widgets

import (
	"fmt"

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
			LabelTemplate:  w.LabelTemplate,
			SlugTemplate:   w.SlugTemplate,
			NameTemplate:   w.NameTemplate,
			MaxSeries:      w.MaxSeries,
			CleanupMissing: w.CleanupMissing,
		}
		switch {
		case w.Template == string(pushward.WidgetTemplateStatList):
			rows := make([]StatListRow, 0, len(w.StatRows))
			for _, r := range w.StatRows {
				rows = append(rows, StatListRow{
					Label: r.Label, Query: r.Query, ValueTemplate: r.ValueTemplate,
					Unit: r.Unit, MissingValue: r.MissingValue,
				})
			}
			src, err := NewStatListSource(mc, rows)
			if err != nil {
				return nil, fmt.Errorf("widget %q: %w", w.Slug, err)
			}
			spec.StatListSource = src
		case w.Query != "":
			spec.Source = &ScalarSource{Client: mc, Expr: w.Query}
		case w.QueryAll != "":
			spec.MultiSource = &MultiSource{Client: mc, Expr: w.QueryAll}
		}
		specs = append(specs, spec)
	}
	return specs, nil
}
