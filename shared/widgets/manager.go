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
	// rejects payloads over 6 rows, so the default is 6. Set to 0 for the
	// default; tests can raise/lower this independently.
	MaxStatRows int

	// StatChangeMask, when non-nil, restricts stat_list change detection to
	// the rows whose mask entry is true. A false entry marks a display-only
	// row: it is still rendered and published, but a change in its value alone
	// never triggers a PATCH - when a triggering row does change, the whole
	// card (including these rows) re-renders with current values. A nil mask
	// (the default) means every row participates, matching the historical
	// "any row changed" behavior. Indexed by row position; rows at indices
	// >= len(mask) always participate, so a mask shorter than the row count
	// still triggers on its trailing rows. A full-length all-false mask is
	// permitted (not rejected at construction) and yields a create-once
	// widget that never PATCHes on a value change, though it still re-renders
	// if the row count changes.
	StatChangeMask []bool

	Interval     time.Duration
	UpdateMode   UpdateMode
	MinChange    float64
	PushThrottle *int
	// StaleAfter is passed straight through to CreateWidget: seconds after the
	// last update before the client renders the widget as stale (60-604800),
	// nil for never. Only applied at create time, like PushThrottle - the
	// manager's PATCHes carry content and never re-tune the widget's config.
	StaleAfter *int
	// Heartbeat re-sends the last published content once a widget has gone this
	// long without a PATCH, so a StaleAfter window does not expire while the
	// metric sits flat. The server treats a PATCH whose merged content matches
	// what it stored as a touch - it re-stamps updated_at without pushing and
	// refunds the quota slot - so the cost is one request.
	//
	// Re-sending the LAST content rather than re-rendering is what makes that
	// hold. A freshly rendered payload can differ even when the value did not:
	// a PointSource's buffer grows and shifts on every poll, so a re-render
	// would defeat the server's equality check and turn every heartbeat into a
	// real APNs push plus a quota slot.
	//
	// Zero disables it, and it is redundant under UpdateAlways. Sends ride the
	// poll ticker, so the real gap between PATCHes is up to one Interval longer
	// than this - see HeartbeatFor for the interval ratio that keeps the gap
	// inside the window.
	Heartbeat time.Duration

	// Content holds the static fields applied to every PATCH (icon, unit,
	// severity, accent colors, min_value, max_value, ...). The Value field is
	// overwritten at each tick from the source.
	Content pushward.WidgetContent

	// LabelTemplate (optional) renders the WidgetContent.Label string from
	// the polled value. Vars: .Value (float64), .Unit (string), .Labels
	// (map[string]string, multi only). When empty, Label is left unset.
	LabelTemplate string

	// Multi-source-only fields.
	SlugTemplate   string // e.g. "users-{{.instance}}"
	NameTemplate   string // e.g. "Users on {{.instance}}"; falls back to Name
	MaxSeries      int    // per-spec cap; 0 -> DefaultMaxSeries
	CleanupMissing bool   // DELETE widgets for series that disappear
	// MissGrace is how many consecutive ticks a series may be absent before it
	// is pruned (and DELETEd when CleanupMissing). It debounces transient
	// scrape gaps and NaN/Inf readings - which surface as an absent series -
	// so a single missed tick no longer churns the widget (DELETE+re-CREATE
	// flicker, redundant pushes). 0 -> DefaultMissGrace.
	MissGrace      int
	parsedSlugTpl  *template.Template
	parsedNameTpl  *template.Template
	parsedLabelTpl *template.Template
	// seriesState is per-series last-value state for multi-source specs.
	// Owned by exactly one supervisor goroutine - no synchronization needed.
	seriesState map[string]seriesState
}

// Default knobs.
const (
	DefaultInterval    = 60 * time.Second
	DefaultMaxSeries   = 20
	DefaultMaxStatRows = 6 // server cap; clients must not exceed
	jitterFraction     = 4 // ticker jitter = interval / jitterFraction (25%)
	// DefaultMissGrace tolerates one transient missing tick before pruning a
	// multi-source series; the series is pruned on the second consecutive miss.
	DefaultMissGrace = 2
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
	wg       sync.WaitGroup
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
	// Multi-source specs derive a per-series name (NameTemplate, then the
	// deterministic sorted-label fallback in renderSlugName); defaulting Name to
	// the shared Slug here would make every series share one display name and
	// render that fallback unreachable.
	if s.Name == "" && s.MultiSource == nil {
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
	// Points reach the payload only through attachPoints, which reads the
	// scalar Source. Left unchecked, a trend spec on any other source shape
	// would build a pointless payload, get a 422 on create and fail Start -
	// taking every other widget in the manager down with it.
	if s.Template == pushward.WidgetTemplateTrend {
		if s.Source == nil {
			return errors.New("template trend requires a scalar Source; MultiSource and StatListSource cannot supply Content.Points")
		}
		if _, ok := s.Source.(PointSource); !ok && len(s.Content.Points) == 0 {
			return errors.New("template trend requires a Source implementing PointSource, or pre-seeded Content.Points")
		}
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
	// Note: an all-false StatChangeMask is intentionally NOT rejected here.
	// statRowsEqualMasked treats rows past the end of the mask as triggers, so a
	// short all-false mask (display-only head, triggering tail) still updates;
	// and a full-length all-false mask is a legitimate "create once, never patch"
	// static widget. Row count is unknown at construction, so the frozen case
	// cannot be proven here - guarding it would false-reject both valid forms.
	if s.MaxSeries == 0 {
		s.MaxSeries = DefaultMaxSeries
	}
	if s.MissGrace == 0 {
		s.MissGrace = DefaultMissGrace
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
// transient empty state). Spec startups fan out concurrently - for N widgets
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
	// Defer create until the first successful poll for gauge/progress
	// templates - the server rejects them without a Value, and we don't
	// want a missing metric at startup to crash-loop the bridge.
	if !ok && requiresInitialValue(spec.Template) {
		logger.Info("widget create deferred until first successful poll", "template", string(spec.Template))
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.runScalar(ctx, spec, logger, scalarState{lastValue: math.NaN(), needsCreate: true})
		}()
		return nil
	}
	content := renderContent(spec.Content, spec.parsedLabelTpl, valueData{Value: initial, Unit: spec.Content.Unit})
	if ok {
		content.Value = pushward.Float64Ptr(initial)
		attachPoints(spec, &content)
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
		m.runScalar(ctx, spec, logger, scalarState{lastValue: lastValue, lastSent: time.Now(), lastContent: content})
	}()
	return nil
}

// scalarState is the per-widget loop state runScalar carries across ticks.
// lastContent is what the server currently holds, so a heartbeat can re-send it
// byte for byte instead of re-rendering.
type scalarState struct {
	lastValue   float64
	lastSent    time.Time
	lastContent pushward.WidgetContent
	needsCreate bool
}

// attachPoints copies the sparkline history off a source that keeps one, so a
// trend widget gets its Points alongside the tick's Value. Sources that do not
// implement PointSource leave the content untouched.
func attachPoints(spec *Spec, content *pushward.WidgetContent) {
	if ps, ok := spec.Source.(PointSource); ok {
		content.Points = ps.Points()
	}
}

// heartbeatDue reports whether an otherwise-unchanged widget has gone long
// enough without a PATCH to need one. A zero lastSent means nothing has been
// sent yet, which is the deferred-create case - there is no stale window to
// keep open until the widget exists.
func heartbeatDue(spec *Spec, lastSent time.Time) bool {
	return spec.Heartbeat > 0 && !lastSent.IsZero() && time.Since(lastSent) >= spec.Heartbeat
}

// HeartbeatFor derives a Spec.Heartbeat from a stale_after window: half the
// window, floored at 30s so the shortest legal window (60s) cannot turn the
// heartbeat into a tight request loop. A nil window means no heartbeat.
//
// Callers must keep their poll Interval at or below staleAfter/3. Sends ride
// the poll ticker, so the worst-case gap between two PATCHes is Heartbeat plus
// one Interval; at that ratio the gap tops out at five sixths of the window and
// the widget never dims. An Interval of staleAfter/2 would instead land the
// refresh exactly on the boundary and dim the widget once per cycle.
func HeartbeatFor(staleAfter *int) time.Duration {
	if staleAfter == nil {
		return 0
	}
	return max(30*time.Second, time.Duration(*staleAfter)*time.Second/2)
}

// requiresInitialValue reports whether the server's widget-create validation
// demands a non-nil Value for this template. Value/status accept nil; gauge and
// progress require a number alongside the bounds, and trend requires one
// alongside its points.
//
// A trend widget also needs Content.Points, which the plain source interfaces
// do not produce: either the Source implements PointSource, or the caller seeds
// the points in the spec's static Content. Without either, the create is
// rejected. A PointSource that is still filling its buffer should return
// ErrNoData, which holds the create here until it has enough samples.
func requiresInitialValue(t pushward.WidgetTemplate) bool {
	switch t {
	case pushward.WidgetTemplateGauge, pushward.WidgetTemplateProgress, pushward.WidgetTemplateTrend:
		return true
	default:
		return false
	}
}

func (m *Manager) runScalar(ctx context.Context, spec *Spec, logger *slog.Logger, st scalarState) {
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
			content := renderContent(spec.Content, spec.parsedLabelTpl, valueData{Value: v, Unit: spec.Content.Unit})
			content.Value = pushward.Float64Ptr(v)
			attachPoints(spec, &content)
			if st.needsCreate {
				if err := m.createWidget(ctx, spec, spec.Slug, spec.Name, content); err != nil {
					if !errors.Is(err, context.Canceled) {
						logger.Warn("deferred widget create failed", "error", err)
					}
					continue
				}
				st.needsCreate = false
				st.lastValue, st.lastContent, st.lastSent = v, content, time.Now()
				continue
			}
			unchanged := !math.IsNaN(st.lastValue) && spec.UpdateMode != UpdateAlways &&
				!valueChanged(st.lastValue, v, spec.MinChange)
			if unchanged && !heartbeatDue(spec, st.lastSent) {
				continue
			}
			// On a heartbeat the payload is the stored content, not the tick's:
			// the value did not move, so anything different in a re-render (a
			// grown PointSource buffer) would read as a real change server-side
			// and cost a push. lastValue also stays put, because it names the
			// value the server actually holds.
			send := content
			if unchanged {
				send = st.lastContent
			}
			if err := m.pwClient.UpdateWidget(ctx, spec.Slug, pushward.UpdateWidgetRequest{Content: &send}); err != nil {
				if !errors.Is(err, context.Canceled) {
					logger.Warn("widget update failed", "error", err)
				}
				continue
			}
			if !unchanged {
				st.lastValue, st.lastContent = v, content
			}
			st.lastSent = time.Now()
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
		m.runStatList(ctx, spec, logger, rows, time.Now())
	}()
	return nil
}

func (m *Manager) runStatList(ctx context.Context, spec *Spec, logger *slog.Logger, lastRows []pushward.StatRow, lastSent time.Time) {
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
			unchanged := spec.UpdateMode != UpdateAlways && statRowsEqualMasked(lastRows, rows, spec.StatChangeMask)
			if unchanged && !heartbeatDue(spec, lastSent) {
				continue
			}
			content := spec.Content
			// A heartbeat re-sends the stored rows. Under a StatChangeMask
			// "unchanged" only means no triggering row moved, so the fresh rows
			// can still differ in a display-only column - sending those would
			// cost a push the mask exists to avoid.
			content.StatRows = rows
			if unchanged {
				content.StatRows = lastRows
			}
			if err := m.pwClient.UpdateWidget(ctx, spec.Slug, pushward.UpdateWidgetRequest{Content: &content}); err != nil {
				if !errors.Is(err, context.Canceled) {
					logger.Warn("widget update failed", "error", err)
				}
				continue
			}
			if !unchanged {
				lastRows = rows
			}
			lastSent = time.Now()
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

// statRowEqual compares one row's rendered fields. Timer is compared by value
// rather than pointer identity, so a source that rebuilds the timer every tick
// does not read as changed on every poll.
func statRowEqual(a, b pushward.StatRow) bool {
	if a.Label != b.Label || a.Value != b.Value || a.Unit != b.Unit {
		return false
	}
	if a.Timer == nil || b.Timer == nil {
		return a.Timer == b.Timer
	}
	return a.Timer.Style == b.Timer.Style && a.Timer.Date.Equal(b.Timer.Date)
}

// statRowsEqual reports whether two stat-row slices carry identical rows by
// position. Returns true for two nil slices.
func statRowsEqual(a, b []pushward.StatRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !statRowEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// statRowsEqualMasked reports whether the change-detection-relevant rows of a
// and b are identical. A nil mask delegates to statRowsEqual (every row
// counts). Otherwise only rows whose mask entry is true are compared; rows
// past the end of the mask default to participating, so a short mask never
// silently freezes extra rows. A length mismatch between a and b always counts
// as changed (a row appeared or disappeared), matching statRowsEqual.
func statRowsEqualMasked(a, b []pushward.StatRow, mask []bool) bool {
	if mask == nil {
		return statRowsEqual(a, b)
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if i < len(mask) && !mask[i] {
			continue // display-only row
		}
		if !statRowEqual(a[i], b[i]) {
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
		// Non-fatal - supervisor will retry next tick.
		logger.Warn("multi-source initial poll failed", "error", err)
	}
	if err := m.applyMulti(ctx, spec, logger, values /*firstTime=*/, true); err != nil {
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
			spec.seriesState[slug] = seriesState{lastValue: lv.Value, hasValue: true, lastSent: time.Now()}
			continue
		}

		// No stored-content dance here, unlike the scalar and stat_list loops:
		// multi content is a pure function of the value, the labels and the
		// static Content, and attachPoints never runs on this path, so an
		// unchanged value re-renders to a byte-identical payload already.
		changed := !state.hasValue || valueChanged(state.lastValue, lv.Value, spec.MinChange)
		if spec.UpdateMode == UpdateAlways || changed || heartbeatDue(spec, state.lastSent) {
			if err := m.pwClient.UpdateWidget(ctx, slug, pushward.UpdateWidgetRequest{Content: &content}); err != nil {
				if !errors.Is(err, context.Canceled) {
					logger.Warn("widget update failed", "slug", slug, "error", err)
				}
				continue
			}
			spec.seriesState[slug] = seriesState{lastValue: lv.Value, hasValue: true, lastSent: time.Now()}
		} else if state.missCount != 0 {
			// Seen but unchanged: clear any accumulated miss streak so a prior
			// transient gap doesn't later trigger a premature prune.
			state.missCount = 0
			spec.seriesState[slug] = state
		}
	}

	// Prune missing series from the in-memory map so it can't accumulate
	// dead entries indefinitely under cardinality churn; only DELETE the
	// server-side widget when CleanupMissing is set. A grace window
	// (spec.MissGrace) absorbs transient scrape gaps / NaN readings so a single
	// missing tick doesn't churn the widget.
	for slug, st := range spec.seriesState {
		if _, present := seen[slug]; present {
			continue
		}
		st.missCount++
		if st.missCount < spec.MissGrace {
			spec.seriesState[slug] = st // keep lastValue; record the miss
			continue
		}
		if spec.CleanupMissing {
			if err := m.pwClient.DeleteWidget(ctx, slug); err != nil {
				// A cancelled ctx is the normal shutdown path, not a failure -
				// don't log it as one (matches the sibling update paths).
				if !errors.Is(err, context.Canceled) {
					logger.Warn("failed to delete missing widget", "slug", slug, "error", err)
				}
				spec.seriesState[slug] = st // retain so the delete is retried next tick
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
		StaleAfter:   spec.StaleAfter,
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
	missCount int       // consecutive ticks this series has been absent; reset when seen
	lastSent  time.Time // when this series last PATCHed, for Spec.Heartbeat
}
