// Package widgets wires the grafana integration's Prometheus client into the
// generic widget poller. The shared shared/widgets package owns the polling
// loop and change-detection logic; this package just adapts metrics.Client
// to the ValueSource / MultiValueSource interfaces.
package widgets

import (
	"context"
	"time"

	"github.com/mac-lucky/pushward-integrations/grafana/internal/metrics"
	sharedwidgets "github.com/mac-lucky/pushward-integrations/shared/widgets"
)

// ScalarSource wraps metrics.Client.QueryInstant to expose a single scalar
// value per call. Returns shared/widgets.ErrNoData when the query has no
// result (the manager skips that tick rather than treating it as an error).
type ScalarSource struct {
	Client *metrics.Client
	Expr   string
}

// Value implements shared/widgets.ValueSource.
func (s *ScalarSource) Value(ctx context.Context) (float64, error) {
	point, err := s.Client.QueryInstant(ctx, s.Expr, time.Now())
	if err != nil {
		return 0, err
	}
	if point == nil {
		return 0, sharedwidgets.ErrNoData
	}
	return point.V, nil
}

// MultiSource wraps metrics.Client.QueryInstantAll to expose label-keyed
// fan-out values. Use this for queries that return multiple series (one
// widget per series).
type MultiSource struct {
	Client *metrics.Client
	Expr   string
}

// Values implements shared/widgets.MultiValueSource.
func (s *MultiSource) Values(ctx context.Context) ([]sharedwidgets.LabeledValue, error) {
	points, err := s.Client.QueryInstantAll(ctx, s.Expr, time.Now())
	if err != nil {
		return nil, err
	}
	out := make([]sharedwidgets.LabeledValue, 0, len(points))
	for _, p := range points {
		out = append(out, sharedwidgets.LabeledValue{Labels: p.Labels, Value: p.Point.V})
	}
	return out, nil
}
