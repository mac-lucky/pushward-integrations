package widgets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// UpdateMode controls when a tick produces a PATCH.
type UpdateMode string

const (
	// UpdateOnChange (default) skips PATCH when the new value equals the
	// last-sent value (within MinChange tolerance).
	UpdateOnChange UpdateMode = "on_change"
	// UpdateAlways sends a PATCH on every tick.
	UpdateAlways UpdateMode = "always"
)

// Spec declares one widget the Manager should keep in sync.
//
// Exactly one of Source / MultiSource must be set. Scalar sources produce a
// single widget identified by Slug. Multi sources produce one widget per
// LabeledValue returned, with slugs rendered from SlugTemplate against the
// returned Labels (e.g. SlugTemplate "users-{{.instance}}").
type Spec struct {
	// Slug is the per-user widget identifier for scalar widgets, or the
	// slug-template input for multi sources when SlugTemplate is empty.
	// Multi-source specs prefer the templated form.
	Slug string

	// Name is the human-readable widget name shown in the iOS picker.
	// For multi sources, NameTemplate is applied per series.
	Name string

	Template       pushward.WidgetTemplate
	Source         ValueSource
	MultiSource    MultiValueSource
	StatListSource StatListSource
	// MaxStatRows caps the rows accepted from StatListSource; the server
	// rejects payloads over 4 rows, so the default is 4. Set to 0 for the
	// default; tests can raise/lower this independently.
	MaxStatRows  int
	Interval     time.Duration
	UpdateMode   UpdateMode
	MinChange    float64
	PushThrottle *int

	// Content holds the static fields applied to every PATCH (icon, unit,
	// severity, accent colors, min_value, max_value, …). The Value field is
	// overwritten at each tick from the source.
	Content pushward.WidgetContent

	// LabelTemplate (optional) renders the WidgetContent.Label string from
	// the polled value. Vars: .Value (float64), .Unit (string), .Labels
	// (map[string]string, multi only). When empty, Label is left unset.
	LabelTemplate string

	// Multi-source-only fields.
	SlugTemplate    string // e.g. "users-{{.instance}}"
	NameTemplate    string // e.g. "Users on {{.instance}}"; falls back to Name
	MaxSeries       int    // per-spec cap; 0 → DefaultMaxSeries
	CleanupMissing  bool   // DELETE widgets for series that disappear
	parsedSlugTpl   *template.Template
	parsedNameTpl   *template.Template
	parsedLabelTpl  *template.Template
	// seriesState is per-series last-value state for multi-source specs.
	// Owned by exactly one supervisor goroutine — no synchronization needed.
	seriesState map[string]seriesState
}

// Default knobs.
const (
	DefaultInterval    = 60 * time.Second
	DefaultMaxSeries   = 20
	DefaultMaxStatRows = 4 // server cap; clients must not exceed
	jitterFraction     = 4 // ticker jitter = interval / jitterFraction (25%)
)

// Manager runs one polling goroutine per scalar widget (and one supervisor
// goroutine per multi-source spec) until its context is cancelled.
//
// Concurrency model: each scalar widget's lastValue is goroutine-local;
// multi-source supervisors own their own per-series state map. The Manager
// itself only mutates state during Start (single-threaded) and Wait
// (read-only).
type Manager struct {
	pwClient *pushward.Client
	specs    []*Spec
	logger   *slog.Logger
	wg sync.WaitGroup
	// cancel cleans up the internal context if Start fails after some
	// specs have already spawned their polling goroutine. After a
	// successful Start the cancel is implicit through the parent context;
	// callers stop the manager by cancelling that.
	cancel context.CancelFunc
}

// New validates and prepares specs but does not start any goroutines.
// Returns an error if any spec is malformed.
func New(pwClient *pushward.Client, specs []Spec, logger *slog.Logger) (*Manager, error) {
	if pwClient == nil {
		return nil, errors.New("widgets: pushward client is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	prepared := make([]*Spec, 0, len(specs))
	for i := range specs {
		s := specs[i]
		if err := prepare(&s); err != nil {
			return nil, fmt.Errorf("widget %q: %w", s.Slug, err)
		}
		prepared = append(prepared, &s)
	}
	return &Manager{pwClient: pwClient, specs: prepared, logger: logger}, nil
}

// exactlyOneSource asserts the Spec's source fields are mutually exclusive.
func exactlyOneSource(s *Spec) bool {
	n := 0
	if s.Source != nil {
		n++
	}
	if s.MultiSource != nil {
		n++
	}
	if s.StatListSource != nil {
		n++
	}
	return n == 1
}

func prepare(s *Spec) error {
	if s.Slug == "" {
		return errors.New("slug is required")
	}
	if s.Name == "" {
		s.Name = s.Slug
	}
	if !exactlyOneSource(s) {
		return errors.New("exactly one of Source, MultiSource, or StatListSource must be set")
	}
	if s.Template == "" {
		if s.StatListSource != nil {
			s.Template = pushward.WidgetTemplateStatList
		} else {
			s.Template = pushward.WidgetTemplateValue
		}
	}
	if s.Template == pushward.WidgetTemplateStatList && s.StatListSource == nil {
		return errors.New("template stat_list requires StatListSource")
	}
	if s.StatListSource != nil && s.Template != pushward.WidgetTemplateStatList {
		return fmt.Errorf("StatListSource only valid with template stat_list, got %q", s.Template)
	}
	if s.MaxStatRows == 0 {
		s.MaxStatRows = DefaultMaxStatRows
	}
	if s.Interval <= 0 {
		s.Interval = DefaultInterval
	}
	if s.UpdateMode == "" {
		s.UpdateMode = UpdateOnChange
	}
	if s.MaxSeries == 0 {
		s.MaxSeries = DefaultMaxSeries
	}
	if s.LabelTemplate != "" {
		tpl, err := template.New("label").Option("missingkey=zero").Parse(s.LabelTemplate)
		if err != nil {
			return fmt.Errorf("parsing label_template: %w", err)
		}
		s.parsedLabelTpl = tpl
	}
	if s.MultiSource != nil {
		if s.SlugTemplate == "" {
			return errors.New("multi-source widgets require slug_template")
		}
		if !strings.Contains(s.SlugTemplate, "{{") {
			return errors.New("slug_template must reference at least one label, e.g. {{.instance}}")
		}
		// missingkey=error: typos in slug templates must fail loudly rather
		// than silently produce a slug like "users-" with the label gone.
		tpl, err := template.New("slug").Option("missingkey=error").Parse(s.SlugTemplate)
		if err != nil {
			return fmt.Errorf("parsing slug_template: %w", err)
		}
		s.parsedSlugTpl = tpl
		if s.NameTemplate != "" {
			ntpl, err := template.New("name").Option("missingkey=zero").Parse(s.NameTemplate)
			if err != nil {
				return fmt.Errorf("parsing name_template: %w", err)
			}
			s.parsedNameTpl = ntpl
		}
		s.seriesState = make(map[string]seriesState)
	}
	return nil
}

// Start spawns one goroutine per scalar widget and one supervisor per
// multi-source / stat_list spec. The first poll for each spec runs
// synchronously so the initial widget creation includes a real value (no
// transient empty state). Spec startups fan out concurrently — for N widgets
// this is one CreateWidget round-trip wall-clock instead of N.
//
// Returns the first fatal startup error (e.g. widget-limit exceeded); on
// failure the manager's context is cancelled so any specs that already
// spawned their goroutine drain cleanly. Per-tick query errors are logged,
// never surfaced here.
func (m *Manager) Start(ctx context.Context) error {
	gCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	errs := make(chan error, len(m.specs))
	var startWG sync.WaitGroup
	for _, spec := range m.specs {
		spec := spec
		startWG.Add(1)
		go func() {
			defer startWG.Done()
			errs <- m.startOne(gCtx, spec)
		}()
	}
	startWG.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			cancel()
			return err
		}
	}
	return nil
}

// startOne dispatches to the per-mode startup helper. Each helper performs
// the initial poll, idempotent CreateWidget, and spawns the supervisor
// goroutine.
func (m *Manager) startOne(ctx context.Context, spec *Spec) error {
	switch {
	case spec.Source != nil:
		return m.startScalar(ctx, spec)
	case spec.MultiSource != nil:
		return m.startMulti(ctx, spec)
	case spec.StatListSource != nil:
		return m.startStatList(ctx, spec)
	}
	return fmt.Errorf("widget %q: no source configured", spec.Slug)
}

// Wait blocks until all goroutines have exited. Call after Start; cancel
// the parent context passed to Start to trigger shutdown.
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) startScalar(ctx context.Context, spec *Spec) error {
	logger := m.logger.With("widget", spec.Slug)
	initial, ok, err := pollScalar(ctx, spec, logger)
	if err != nil {
		return fmt.Errorf("widget %q initial poll failed fatally: %w", spec.Slug, err)
	}
	content := renderContent(spec.Content, spec.parsedLabelTpl, valueData{Value: initial, Unit: spec.Content.Unit})
	if ok {
		content.Value = pushward.Float64Ptr(initial)
	}
	if err := m.createWidget(ctx, spec, spec.Slug, spec.Name, content); err != nil {
		return err
	}
	// NaN sentinel marks "no value yet"; the first successful tick wins the
	// initial PATCH unconditionally because NaN != anything else.
	lastValue := math.NaN()
	if ok {
		lastValue = initial
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.runScalar(ctx, spec, logger, lastValue)
	}()
	return nil
}

func (m *Manager) runScalar(ctx context.Context, spec *Spec, logger *slog.Logger, lastValue float64) {
	waitJitter(ctx, spec.Interval)
	if ctx.Err() != nil {
		return
	}
	ticker := time.NewTicker(spec.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			v, ok := tickScalar(ctx, spec, logger)
			if !ok {
				continue
			}
			if !math.IsNaN(lastValue) && spec.UpdateMode != UpdateAlways && !valueChanged(lastValue, v, spec.MinChange) {
				continue
			}
			content := renderContent(spec.Content, spec.parsedLabelTpl, valueData{Value: v, Unit: spec.Content.Unit})
			content.Value = pushward.Float64Ptr(v)
			if err := m.pwClient.UpdateWidget(ctx, spec.Slug, pushward.UpdateWidgetRequest{Content: &content}); err != nil {
				if !errors.Is(err, context.Canceled) {
					logger.Warn("widget update failed", "error", err)
				}
				continue
			}
			lastValue = v
		}
	}
}

func (m *Manager) startStatList(ctx context.Context, spec *Spec) error {
	logger := m.logger.With("widget", spec.Slug, "template", "stat_list")
	rows, err := pollStatList(ctx, spec)
	if err != nil && !errors.Is(err, ErrNoData) {
		// Non-fatal at startup; widget is created with whatever rows we have
		// (possibly empty). The runner retries every tick.
		logger.Warn("stat_list initial poll failed", "error", err)
	}
	rows = trimStatRows(rows, spec.MaxStatRows)

	content := spec.Content
	content.StatRows = rows
	if err := m.createWidget(ctx, spec, spec.Slug, spec.Name, content); err != nil {
		return err
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.runStatList(ctx, spec, logger, rows)
	}()
	return nil
}

func (m *Manager) runStatList(ctx context.Context, spec *Spec, logger *slog.Logger, lastRows []pushward.StatRow) {
	waitJitter(ctx, spec.Interval)
	if ctx.Err() != nil {
		return
	}
	ticker := time.NewTicker(spec.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rows, err := pollStatList(ctx, spec)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					logger.Warn("stat_list poll failed", "error", err)
				}
				continue
			}
			rows = trimStatRows(rows, spec.MaxStatRows)
			if spec.UpdateMode != UpdateAlways && statRowsEqual(lastRows, rows) {
				continue
			}
			content := spec.Content
			content.StatRows = rows
			if err := m.pwClient.UpdateWidget(ctx, spec.Slug, pushward.UpdateWidgetRequest{Content: &content}); err != nil {
				if !errors.Is(err, context.Canceled) {
					logger.Warn("widget update failed", "error", err)
				}
				continue
			}
			lastRows = rows
		}
	}
}

func pollStatList(ctx context.Context, spec *Spec) ([]pushward.StatRow, error) {
	rows, err := spec.StatListSource.Rows(ctx)
	if err != nil {
		if errors.Is(err, ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	return rows, nil
}

// trimStatRows enforces the server's row cap; callers may pass MaxStatRows=0
// to use DefaultMaxStatRows. Returns the input unchanged if already within
// the cap.
func trimStatRows(rows []pushward.StatRow, maxRows int) []pushward.StatRow {
	if maxRows <= 0 {
		maxRows = DefaultMaxStatRows
	}
	if len(rows) <= maxRows {
		return rows
	}
	return rows[:maxRows]
}

// statRowsEqual reports whether two stat-row slices are byte-identical
// label/value/unit by position. Returns true for two nil slices.
func statRowsEqual(a, b []pushward.StatRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (m *Manager) startMulti(ctx context.Context, spec *Spec) error {
	logger := m.logger.With("widget_group", spec.Slug)
	// Initial poll: collect values, ensure widget instances exist.
	values, err := pollMulti(ctx, spec)
	if err != nil {
		// Non-fatal — supervisor will retry next tick.
		logger.Warn("multi-source initial poll failed", "error", err)
	}
	if err := m.applyMulti(ctx, spec, logger, values, /*firstTime=*/ true); err != nil {
		return err
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.runMulti(ctx, spec, logger)
	}()
	return nil
}

func (m *Manager) runMulti(ctx context.Context, spec *Spec, logger *slog.Logger) {
	waitJitter(ctx, spec.Interval)
	if ctx.Err() != nil {
		return
	}
	ticker := time.NewTicker(spec.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			values, err := pollMulti(ctx, spec)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					logger.Warn("multi-source poll failed", "error", err)
				}
				continue
			}
			_ = m.applyMulti(ctx, spec, logger, values, false)
		}
	}
}

func (m *Manager) applyMulti(ctx context.Context, spec *Spec, logger *slog.Logger, values []LabeledValue, firstTime bool) error {
	seen := make(map[string]struct{}, len(values))
	for _, lv := range values {
		if math.IsNaN(lv.Value) || math.IsInf(lv.Value, 0) {
			continue
		}
		slug, name, err := renderSlugName(spec, lv.Labels)
		if err != nil {
			logger.Warn("failed to render slug/name template", "error", err, "labels", lv.Labels)
			continue
		}
		state, exists := spec.seriesState[slug]
		if !exists && len(spec.seriesState) >= spec.MaxSeries {
			logger.Error("widget series cap reached, dropping new series",
				"slug", slug, "cap", spec.MaxSeries)
			continue
		}
		seen[slug] = struct{}{}

		content := renderContent(spec.Content, spec.parsedLabelTpl, valueData{Value: lv.Value, Unit: spec.Content.Unit, Labels: lv.Labels})
		content.Value = pushward.Float64Ptr(lv.Value)

		if !exists {
			if err := m.createWidget(ctx, spec, slug, name, content); err != nil {
				if firstTime {
					return err
				}
				logger.Warn("failed to create widget for new series", "slug", slug, "error", err)
				continue
			}
			spec.seriesState[slug] = seriesState{lastValue: lv.Value, hasValue: true}
			continue
		}

		changed := !state.hasValue || valueChanged(state.lastValue, lv.Value, spec.MinChange)
		if spec.UpdateMode == UpdateAlways || changed {
			if err := m.pwClient.UpdateWidget(ctx, slug, pushward.UpdateWidgetRequest{Content: &content}); err != nil {
				if !errors.Is(err, context.Canceled) {
					logger.Warn("widget update failed", "slug", slug, "error", err)
				}
				continue
			}
			spec.seriesState[slug] = seriesState{lastValue: lv.Value, hasValue: true}
		}
	}

	// Prune missing series from the in-memory map so it can't accumulate
	// dead entries indefinitely under cardinality churn; only DELETE the
	// server-side widget when CleanupMissing is set.
	for slug := range spec.seriesState {
		if _, present := seen[slug]; present {
			continue
		}
		if spec.CleanupMissing {
			if err := m.pwClient.DeleteWidget(ctx, slug); err != nil {
				logger.Warn("failed to delete missing widget", "slug", slug, "error", err)
				continue
			}
		}
		delete(spec.seriesState, slug)
	}
	return nil
}

func (m *Manager) createWidget(ctx context.Context, spec *Spec, slug, name string, content pushward.WidgetContent) error {
	content.Template = spec.Template
	req := pushward.CreateWidgetRequest{
		Slug:         slug,
		Name:         name,
		Content:      content,
		PushThrottle: spec.PushThrottle,
	}
	if err := m.pwClient.CreateWidget(ctx, req); err != nil {
		var herr *pushward.HTTPError
		if errors.As(err, &herr) && herr.Code == pushward.ErrCodeWidgetLimitExceeded {
			return fmt.Errorf("widget %q create failed: per-user widget cap reached", slug)
		}
		return fmt.Errorf("widget %q create failed: %w", slug, err)
	}
	return nil
}

func pollScalar(ctx context.Context, spec *Spec, logger *slog.Logger) (float64, bool, error) {
	v, err := spec.Source.Value(ctx)
	if err != nil {
		if errors.Is(err, ErrNoData) {
			return 0, false, nil
		}
		if errors.Is(err, context.Canceled) {
			return 0, false, err
		}
		logger.Warn("widget query failed", "error", err)
		return 0, false, nil
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		logger.Warn("widget query returned non-finite value", "value", v)
		return 0, false, nil
	}
	return v, true, nil
}

func tickScalar(ctx context.Context, spec *Spec, logger *slog.Logger) (float64, bool) {
	v, ok, err := pollScalar(ctx, spec, logger)
	if err != nil {
		return 0, false
	}
	return v, ok
}

func pollMulti(ctx context.Context, spec *Spec) ([]LabeledValue, error) {
	values, err := spec.MultiSource.Values(ctx)
	if err != nil {
		if errors.Is(err, ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	return values, nil
}

// valueChanged reports whether newV differs from oldV by more than minChange.
// Two NaN readings count as "no change" to avoid spurious pushes when a
// metric is intermittently unavailable. When minChange is zero, exact float
// inequality is used so integer counters increment-by-one always pushes.
func valueChanged(oldV, newV, minChange float64) bool {
	if math.IsNaN(oldV) && math.IsNaN(newV) {
		return false
	}
	if minChange == 0 {
		return oldV != newV
	}
	return math.Abs(newV-oldV) > minChange
}

// waitJitter sleeps for a random fraction of interval before the first tick
// so concurrent widgets with the same interval don't all fire in lockstep.
// Returns early if the context is cancelled.
func waitJitter(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	maxJitter := int64(interval / jitterFraction)
	if maxJitter <= 0 {
		return
	}
	d := time.Duration(rand.Int64N(maxJitter)) // #nosec G404 -- jitter, not security-sensitive
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

type valueData struct {
	Value  float64
	Unit   string
	Labels map[string]string
}

func renderContent(base pushward.WidgetContent, labelTpl *template.Template, data valueData) pushward.WidgetContent {
	out := base
	if labelTpl != nil {
		var buf bytes.Buffer
		if err := labelTpl.Execute(&buf, data); err == nil {
			out.Label = buf.String()
		}
	}
	return out
}

func renderSlugName(spec *Spec, labels map[string]string) (string, string, error) {
	var slug bytes.Buffer
	if err := spec.parsedSlugTpl.Execute(&slug, labels); err != nil {
		return "", "", err
	}
	name := spec.Name
	if spec.parsedNameTpl != nil {
		var buf bytes.Buffer
		if err := spec.parsedNameTpl.Execute(&buf, labels); err == nil {
			name = buf.String()
		}
	} else if name == "" {
		// Deterministic fallback so widget names are stable across restarts.
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, labels[k])
		}
		name = strings.Join(parts, " ")
	}
	return slug.String(), name, nil
}

type seriesState struct {
	lastValue float64
	hasValue  bool
}
