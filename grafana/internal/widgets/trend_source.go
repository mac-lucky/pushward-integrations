package widgets

import (
	"context"
	"slices"
	"time"

	"github.com/mac-lucky/pushward-integrations/grafana/internal/metrics"
	sharedwidgets "github.com/mac-lucky/pushward-integrations/shared/widgets"
)

// Trend sparkline bounds, mirrored from pushward-server: fewer than 2 or more
// than 48 points is rejected.
const (
	trendMinPoints = 2
	trendMaxPoints = 48
)

// TrendSource is a scalar source that also keeps the rolling sample buffer the
// trend template draws as a sparkline. One instant value is queried per tick; a
// reading that differs from the last always lands in the buffer, and the oldest
// is dropped once it is full.
//
// Value reports ErrNoData until there are trendMinPoints, which leaves the
// manager's widget create deferred rather than posting a payload the server
// would reject. Both methods run on the manager's one goroutine for this
// widget, so the buffer needs no locking.
//
// Deliberately identical to pushward-grafana-plugin's TrendSource, which drives
// the same shared manager off a Grafana datasource. This bridge does have
// metrics.Client.QueryRange and could backfill the sparkline from real history
// on the first poll instead of waiting two intervals - a genuine improvement,
// but one that makes the two supposedly-mirrored implementations diverge, so it
// belongs in its own change.
type TrendSource struct {
	ScalarSource
	repeatGap  time.Duration
	points     []float64
	lastAppend time.Time
}

// NewTrendSource builds a trend source whose repeat gap is sized off the poll
// interval.
func NewTrendSource(mc *metrics.Client, expr string, interval time.Duration) *TrendSource {
	return &TrendSource{ScalarSource: ScalarSource{Client: mc, Expr: expr}, repeatGap: trendRepeatGap(interval)}
}

// trendRepeatGap is how long an unchanged reading is skipped before being
// recorded again. At the 48-point wire cap, one sample per window/48 is the
// coarsest useful rate, and the buffer's window here is interval*48, so the gap
// is the interval with a 60s floor.
//
// It only bites below a one-minute interval, which is exactly the case that
// needs it: at 5s a flat metric flushes 48 minutes of real history out of the
// buffer in four, leaving a sparkline of 48 identical dots.
func trendRepeatGap(interval time.Duration) time.Duration {
	return max(60*time.Second, interval)
}

// Value implements shared/widgets.ValueSource.
func (s *TrendSource) Value(ctx context.Context) (float64, error) {
	v, err := s.ScalarSource.Value(ctx)
	if err != nil {
		return 0, err
	}
	s.push(v, time.Now())
	if len(s.points) < trendMinPoints {
		return 0, sharedwidgets.ErrNoData
	}
	return v, nil
}

// Points implements shared/widgets.PointSource. The copy keeps the live buffer
// from being aliased into a payload that outlives the tick.
func (s *TrendSource) Points() []float64 {
	if len(s.points) < trendMinPoints {
		return nil
	}
	return slices.Clone(s.points)
}

func (s *TrendSource) push(v float64, now time.Time) {
	n := len(s.points)
	// Below the minimum there is no widget yet, so a repeat still has to count:
	// a metric that starts out flat would otherwise never reach two samples and
	// never publish at all.
	if n >= trendMinPoints && s.points[n-1] == v && now.Sub(s.lastAppend) < s.repeatGap {
		return
	}
	s.lastAppend = now
	if n == trendMaxPoints {
		copy(s.points, s.points[1:])
		s.points[trendMaxPoints-1] = v
		return
	}
	s.points = append(s.points, v)
}

// staticSource publishes a widget that never polls. Value always reports
// ErrNoData, which the manager reads as "nothing to update" after the initial
// create - the countdown template renders entirely from its own dates.
type staticSource struct{}

func (staticSource) Value(context.Context) (float64, error) { return 0, sharedwidgets.ErrNoData }
