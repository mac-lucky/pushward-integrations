// Package widgets provides a generic background poller that publishes named
// numeric values to the pushward-server widget API. Integrations supply a
// ValueSource (single scalar) or MultiValueSource (label-keyed fan-out); the
// manager handles the polling loop, change detection, idempotent widget
// creation, and graceful shutdown.
package widgets

import (
	"context"
	"errors"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// ErrNoData signals that a source has no current value (e.g., an empty
// Prometheus result). The manager treats this as "skip this tick" rather
// than an error.
var ErrNoData = errors.New("widgets: no data")

// ValueSource produces a single numeric value for a scalar widget.
type ValueSource interface {
	Value(ctx context.Context) (float64, error)
}

// MultiValueSource produces a label-keyed set of values for fan-out widgets.
// Each returned entry becomes its own widget; the spec's SlugTemplate
// substitutes label values to derive per-series slugs.
type MultiValueSource interface {
	Values(ctx context.Context) ([]LabeledValue, error)
}

// StatListSource produces a list of pre-formatted (Label, Value, Unit) rows
// for a single stat_list widget. The server caps the list at 4 rows; sources
// that return more rows have the tail dropped at the manager.
type StatListSource interface {
	Rows(ctx context.Context) ([]pushward.StatRow, error)
}

// LabeledValue is one entry returned by a MultiValueSource. Labels keys are
// used both for slug-template substitution and for human-readable widget
// names; values must be the resolved metric value.
type LabeledValue struct {
	Labels map[string]string
	Value  float64
}

// ValueSourceFunc adapts a plain function to ValueSource.
type ValueSourceFunc func(ctx context.Context) (float64, error)

// Value implements ValueSource.
func (f ValueSourceFunc) Value(ctx context.Context) (float64, error) { return f(ctx) }

// MultiValueSourceFunc adapts a plain function to MultiValueSource.
type MultiValueSourceFunc func(ctx context.Context) ([]LabeledValue, error)

// Values implements MultiValueSource.
func (f MultiValueSourceFunc) Values(ctx context.Context) ([]LabeledValue, error) {
	return f(ctx)
}

// StatListSourceFunc adapts a plain function to StatListSource.
type StatListSourceFunc func(ctx context.Context) ([]pushward.StatRow, error)

// Rows implements StatListSource.
func (f StatListSourceFunc) Rows(ctx context.Context) ([]pushward.StatRow, error) { return f(ctx) }
