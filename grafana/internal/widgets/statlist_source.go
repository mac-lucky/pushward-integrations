package widgets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/mac-lucky/pushward-integrations/grafana/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	sharedwidgets "github.com/mac-lucky/pushward-integrations/shared/widgets"
)

// defaultMissingValue is rendered when a stat row's query returns no data and
// no per-row override is configured.
const defaultMissingValue = "—"

// StatListRow describes one row of a stat_list widget at the source layer.
// ValueTemplate is a Go template applied to the polled float — vars are
// `.Value` (float64) and `.Unit` (string).
type StatListRow struct {
	Label         string
	Query         string
	ValueTemplate string
	Unit          string
	MissingValue  string
}

// NewStatListSource pre-parses every value template so misconfigurations
// surface at startup, not on first tick.
func NewStatListSource(client *metrics.Client, rows []StatListRow) (sharedwidgets.StatListSource, error) {
	if client == nil {
		return nil, errors.New("stat_list source requires a metrics client")
	}
	if len(rows) == 0 {
		return nil, errors.New("stat_list source requires at least one row")
	}
	compiled := make([]compiledStatRow, len(rows))
	for i, r := range rows {
		switch {
		case r.Label == "":
			return nil, fmt.Errorf("stat_rows[%d]: label is required", i)
		case r.Query == "":
			return nil, fmt.Errorf("stat_rows[%d]: query is required", i)
		case r.ValueTemplate == "":
			return nil, fmt.Errorf("stat_rows[%d]: value_template is required", i)
		}
		tpl, err := template.New(fmt.Sprintf("row%d", i)).Option("missingkey=zero").Parse(r.ValueTemplate)
		if err != nil {
			return nil, fmt.Errorf("stat_rows[%d]: parsing value_template: %w", i, err)
		}
		missing := r.MissingValue
		if missing == "" {
			missing = defaultMissingValue
		}
		compiled[i] = compiledStatRow{
			label: r.Label, query: r.Query, unit: r.Unit, missing: missing, tpl: tpl,
		}
	}
	return &statListSource{client: client, rows: compiled}, nil
}

type compiledStatRow struct {
	label, query, unit, missing string
	tpl                         *template.Template
}

type statListSource struct {
	client *metrics.Client
	rows   []compiledStatRow
}

// Rows fans out the per-row queries concurrently so a stat_list with N rows
// costs roughly one Prometheus round-trip rather than N. Per-row query
// errors render as the row's MissingValue placeholder — a transient blip on
// one query never blanks the entire widget — so the fan-out only needs a
// plain WaitGroup, not errgroup. Capturing now once before the fan-out
// keeps all rows aligned to the same evaluation instant.
func (s *statListSource) Rows(ctx context.Context) ([]pushward.StatRow, error) {
	out := make([]pushward.StatRow, len(s.rows))
	now := time.Now()
	var wg sync.WaitGroup
	for i, row := range s.rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = s.queryRow(ctx, row, now)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// queryRow renders one row's value; query errors that are not ctx
// cancellation are swallowed into the row's MissingValue placeholder.
func (s *statListSource) queryRow(ctx context.Context, row compiledStatRow, now time.Time) pushward.StatRow {
	val, ok, err := queryPoint(ctx, s.client, row.query, now)
	if err != nil {
		return pushward.StatRow{Label: row.label, Value: row.missing, Unit: row.unit}
	}
	rendered := row.missing
	if ok {
		if v := strings.TrimSpace(renderStatValue(row.tpl, val, row.unit)); v != "" {
			rendered = v
		}
	}
	return pushward.StatRow{Label: row.label, Value: rendered, Unit: row.unit}
}

func queryPoint(ctx context.Context, client *metrics.Client, expr string, ts time.Time) (float64, bool, error) {
	point, err := client.QueryInstant(ctx, expr, ts)
	if err != nil {
		return 0, false, err
	}
	if point == nil {
		return 0, false, nil
	}
	return point.V, true, nil
}

func renderStatValue(tpl *template.Template, value float64, unit string) string {
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, struct {
		Value float64
		Unit  string
	}{value, unit}); err != nil {
		return ""
	}
	return buf.String()
}
