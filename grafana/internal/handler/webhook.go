package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	grafanaapi "github.com/mac-lucky/pushward-integrations/grafana/internal/grafana"
	"github.com/mac-lucky/pushward-integrations/grafana/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/grafana/internal/poller"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const (
	alertStatusFiring   = "firing"
	alertStatusResolved = "resolved"

	templateTimeline   = pushward.TemplateTimeline
	defaultWarningIcon = "exclamationmark.triangle.fill"
	resolvedIcon       = "checkmark.circle.fill"
)

// Config holds timeline display and lifecycle settings for the handler.
type Config struct {
	HistoryWindow   time.Duration
	Priority        int
	CleanupDelay    time.Duration
	StaleTimeout    time.Duration
	SeverityLabel   string
	DefaultSeverity string
	Smoothing       *bool
	Scale           string
	Decimals        *int
}

// Handler receives Grafana webhook alert notifications and creates
// PushWard timeline activities with sparkline history.
type Handler struct {
	pwClient      *pushward.Client
	metricsClient *metrics.Client
	grafanaClient *grafanaapi.Client // nil if auto-extract disabled
	poller        *poller.Poller
	cfg           Config

	mu     sync.Mutex
	active map[string]*alertState
	wg     sync.WaitGroup // tracks in-flight async webhook processing
}

type alertState struct {
	slug         string
	expr         string
	refID        string
	seriesLabel  string
	lastSeen     time.Time
	fingerprints map[string]struct{}
}

func NewHandler(
	pwClient *pushward.Client,
	metricsClient *metrics.Client,
	grafanaClient *grafanaapi.Client,
	p *poller.Poller,
	cfg Config,
) *Handler {
	if cfg.SeverityLabel == "" {
		cfg.SeverityLabel = "severity"
	}
	if cfg.DefaultSeverity == "" {
		cfg.DefaultSeverity = "warning"
	}
	return &Handler{
		pwClient:      pwClient,
		metricsClient: metricsClient,
		grafanaClient: grafanaClient,
		poller:        p,
		cfg:           cfg,
		active:        make(map[string]*alertState),
	}
}

type webhookPayload struct {
	Status string  `json:"status"`
	Alerts []alert `json:"alerts"`
}

type alert struct {
	Status       string             `json:"status"`
	Labels       map[string]string  `json:"labels"`
	Annotations  map[string]string  `json:"annotations"`
	Values       map[string]float64 `json:"values"`
	StartsAt     string             `json:"startsAt"`
	Fingerprint  string             `json:"fingerprint"`
	GeneratorURL string             `json:"generatorURL"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload webhookPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&payload); err != nil {
		slog.Warn("failed to decode webhook payload", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Respond immediately so Grafana doesn't retry on slow Prometheus queries.
	w.WriteHeader(http.StatusOK)

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ctx := context.WithoutCancel(r.Context())
		for _, a := range payload.Alerts {
			switch a.Status {
			case alertStatusFiring:
				h.handleFiring(ctx, a)
			case alertStatusResolved:
				h.handleResolved(ctx, a)
			}
		}
	}()
}

func (h *Handler) handleFiring(ctx context.Context, a alert) {
	alertname := a.Labels["alertname"]
	if alertname == "" {
		alertname = "Grafana Alert"
	}
	slug := makeSlug(alertname)
	logger := slog.With("alertname", alertname, "slug", slug, "fingerprint", a.Fingerprint)

	seriesLabel := a.Annotations["pushward_series_label"]

	// Check-and-mark: reserve the slot before releasing the lock to prevent
	// duplicate creates from concurrent webhooks.
	h.mu.Lock()
	existing := h.active[alertname]
	isNew := existing == nil
	if isNew {
		h.active[alertname] = &alertState{
			slug: slug, seriesLabel: seriesLabel, lastSeen: time.Now(),
			fingerprints: map[string]struct{}{a.Fingerprint: {}},
		}
	} else {
		existing.lastSeen = time.Now()
		existing.fingerprints[a.Fingerprint] = struct{}{}
	}
	h.mu.Unlock()

	expr, refID := h.resolveQuery(ctx, a)

	if isNew {
		err := h.pwClient.CreateActivity(ctx, slug, alertname, h.cfg.Priority,
			int(h.cfg.CleanupDelay.Seconds()), int(h.cfg.StaleTimeout.Seconds()))
		if err != nil {
			h.mu.Lock()
			delete(h.active, alertname)
			h.mu.Unlock()
			logger.Error("failed to create activity", "error", err)
			return
		}
		logger.Info("activity created")

		h.mu.Lock()
		h.active[alertname].expr = expr
		h.active[alertname].refID = refID
		h.mu.Unlock()
	}

	severity := h.resolveSeverity(a)

	// For new alerts, fetch history first so we can derive current values
	// with proper metric labels instead of Grafana expression ref IDs (B, C).
	var history map[string][]pushward.HistoryPoint
	if isNew && expr != "" {
		history = h.fetchHistoryAll(ctx, logger, expr, seriesLabel)
	}

	var values map[string]float64
	if len(history) > 0 {
		values = latestValues(history)
	} else {
		values = h.resolveValues(a, refID, seriesLabel)
	}

	if len(values) == 0 {
		logger.Warn("no values available, skipping initial update (poller will populate)",
			"expr_resolved", expr != "", "refID", refID)
		if isNew && expr != "" {
			h.poller.Start(slug, expr, seriesLabel)
			logger.Info("poller started")
		}
		return
	}

	content := h.buildContent(a, severity, values)
	content.History = history

	err := h.pwClient.UpdateActivity(ctx, slug, pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	})
	if err != nil {
		logger.Error("failed to update activity", "error", err)
		return
	}

	if isNew && expr != "" {
		h.poller.Start(slug, expr, seriesLabel)
		logger.Info("poller started")
	}
}

func (h *Handler) handleResolved(ctx context.Context, a alert) {
	alertname := a.Labels["alertname"]
	if alertname == "" {
		alertname = "Grafana Alert"
	}
	slug := makeSlug(alertname)
	logger := slog.With("alertname", alertname, "slug", slug, "fingerprint", a.Fingerprint)

	h.mu.Lock()
	state, exists := h.active[alertname]
	if !exists {
		h.mu.Unlock()
		return
	}

	delete(state.fingerprints, a.Fingerprint)

	if len(state.fingerprints) > 0 {
		remaining := len(state.fingerprints)
		h.mu.Unlock()
		logger.Info("instance resolved, other instances still firing", "remaining", remaining)
		return
	}

	// All instances resolved — capture state and clean up.
	refID := state.refID
	seriesLabel := state.seriesLabel
	delete(h.active, alertname)
	h.mu.Unlock()

	h.poller.Stop(slug)

	severity := h.resolveSeverity(a)
	values := h.resolveValues(a, refID, seriesLabel)
	if len(values) == 0 {
		values = map[string]float64{"value": 0}
	}
	content := h.buildContent(a, severity, values)
	content.Icon = resolvedIcon
	content.AccentColor = pushward.ColorGreen

	err := h.pwClient.UpdateActivity(ctx, slug, pushward.UpdateRequest{
		State:   pushward.StateEnded,
		Content: content,
	})
	if err != nil {
		logger.Error("failed to end activity", "error", err)
		return
	}

	logger.Info("activity ended")
}

func (h *Handler) resolveQuery(ctx context.Context, a alert) (expr, refID string) {
	if q, ok := a.Annotations["pushward_query"]; ok && q != "" {
		refID = a.Annotations["pushward_ref_id"]
		return q, refID
	}

	if h.grafanaClient != nil {
		ruleUID := grafanaapi.ExtractRuleUID(a.GeneratorURL)
		if ruleUID != "" {
			rq, err := h.grafanaClient.GetRuleQuery(ctx, ruleUID)
			if err != nil {
				slog.Warn("failed to auto-extract query", "rule_uid", ruleUID, "error", err)
			} else {
				return rq.Expr, rq.RefID
			}
		}
	}

	return "", ""
}

func (h *Handler) resolveSeverity(a alert) string {
	if sev, ok := a.Labels[h.cfg.SeverityLabel]; ok {
		switch sev {
		case pushward.SeverityCritical, pushward.SeverityWarning, pushward.SeverityInfo:
			return sev
		}
	}
	return h.cfg.DefaultSeverity
}

// resolveValues builds a multi-key value map from the webhook payload.
// For single-series alerts it returns a single-key map; for multi-series it returns N keys.
func (h *Handler) resolveValues(a alert, preferredRefID, seriesLabel string) map[string]float64 {
	if len(a.Values) == 0 {
		return nil
	}

	if rid := a.Annotations["pushward_ref_id"]; rid != "" {
		preferredRefID = rid
	}

	label := a.Labels["alertname"]
	if label == "" {
		label = "Value"
	}

	// Single ref ID match — use alertname as key for backward compatibility.
	if preferredRefID != "" {
		if v, ok := a.Values[preferredRefID]; ok {
			return map[string]float64{label: v}
		}
	}

	// Single value — use alertname as key.
	if len(a.Values) == 1 {
		for _, v := range a.Values {
			return map[string]float64{label: v}
		}
	}

	// Multi-value: use the webhook's ref ID keys directly.
	// These are Grafana expression ref IDs (A, B, C...), not ideal series labels,
	// but the poller will replace them with proper metric-derived keys on next tick.
	result := make(map[string]float64, len(a.Values))
	for k, v := range a.Values {
		result[k] = v
	}
	return result
}

func latestValues(history map[string][]pushward.HistoryPoint) map[string]float64 {
	values := make(map[string]float64, len(history))
	for key, points := range history {
		if len(points) > 0 {
			values[key] = points[len(points)-1].V
		}
	}
	return values
}

func (h *Handler) buildContent(a alert, severity string, values map[string]float64) pushward.Content {
	var value any
	if len(values) > 0 {
		value = values
	}

	content := pushward.Content{
		Template:    templateTimeline,
		Value:       value,
		Subtitle:    "Grafana",
		AccentColor: pushward.SeverityColor(severity),
		Icon:        pushward.SeverityIcon(severity, defaultWarningIcon),
		Scale:       h.cfg.Scale,
		Smoothing:   h.cfg.Smoothing,
		Decimals:    h.cfg.Decimals,
	}

	if v, ok := a.Annotations["pushward_unit"]; ok {
		content.Unit = v
	}

	if v, ok := a.Annotations["pushward_threshold"]; ok {
		if f, ok := parseAnnotationThreshold(v); ok {
			content.Thresholds = []pushward.Threshold{{
				Value: f,
				Color: pushward.SeverityColor(severity),
			}}
		}
	}

	if summary, ok := a.Annotations["summary"]; ok {
		content.State = summary
	} else {
		content.State = a.Labels["alertname"]
	}

	return content
}

func (h *Handler) fetchHistoryAll(ctx context.Context, logger *slog.Logger, expr, seriesLabel string) map[string][]pushward.HistoryPoint {
	now := time.Now()
	step := h.cfg.HistoryWindow / 120
	if step < 15*time.Second {
		step = 15 * time.Second
	}

	allSeries, err := h.metricsClient.QueryRangeAll(ctx, expr, now.Add(-h.cfg.HistoryWindow), now, step)
	if err != nil {
		logger.Warn("failed to fetch history", "error", err)
		return nil
	}

	if len(allSeries) == 0 {
		return nil
	}

	history := make(map[string][]pushward.HistoryPoint, len(allSeries))
	for _, s := range allSeries {
		key := metrics.SeriesKey(s.Labels, seriesLabel)
		history[key] = s.Points
	}
	return history
}

func makeSlug(alertname string) string {
	return text.SlugHash("grafana", alertname, 8)
}

// StartSweeper runs a background goroutine that removes stale entries from
// the active alerts map when Grafana fails to send a resolved webhook.
func (h *Handler) StartSweeper(ctx context.Context, maxAge time.Duration) {
	if maxAge <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(maxAge / 2)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.sweepStale(maxAge)
			}
		}
	}()
}

func (h *Handler) sweepStale(maxAge time.Duration) {
	now := time.Now()
	h.mu.Lock()
	for name, state := range h.active {
		if now.Sub(state.lastSeen) > maxAge {
			slog.Info("sweeping stale alert", "alertname", name, "slug", state.slug)
			h.poller.Stop(state.slug)
			delete(h.active, name)
		}
	}
	h.mu.Unlock()
}

// StartAlertChecker runs a background goroutine that periodically queries
// the Grafana alertmanager API to detect resolved alerts when webhooks are
// missed. Requires the Grafana client to be configured.
func (h *Handler) StartAlertChecker(ctx context.Context, interval time.Duration) {
	if h.grafanaClient == nil || interval <= 0 {
		return
	}
	slog.Info("alert state checker enabled", "interval", interval)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.checkAlertStates(ctx)
			}
		}
	}()
}

func (h *Handler) checkAlertStates(ctx context.Context) {
	h.mu.Lock()
	type entry struct {
		name  string
		state alertState // copy
	}
	entries := make([]entry, 0, len(h.active))
	for name, state := range h.active {
		entries = append(entries, entry{name: name, state: *state})
	}
	h.mu.Unlock()

	for _, e := range entries {
		firing, err := h.grafanaClient.IsAlertFiring(ctx, e.name)
		if err != nil {
			slog.Warn("alert state check failed", "alertname", e.name, "error", err)
			continue
		}
		if firing {
			continue
		}

		// Alert is no longer firing — end the activity.
		h.mu.Lock()
		_, stillActive := h.active[e.name]
		if stillActive {
			delete(h.active, e.name)
		}
		h.mu.Unlock()

		if !stillActive {
			continue // already resolved by webhook while we were checking
		}

		h.poller.Stop(e.state.slug)
		h.endAlertActivity(ctx, e.name, &e.state)
	}
}

// endAlertActivity sends an ENDED update for an alert that is no longer firing.
func (h *Handler) endAlertActivity(ctx context.Context, alertname string, state *alertState) {
	logger := slog.With("alertname", alertname, "slug", state.slug)

	// Fetch current metric values for the end update.
	var values map[string]float64
	if state.expr != "" {
		points, err := h.metricsClient.QueryInstantAll(ctx, state.expr, time.Now())
		if err == nil {
			values = make(map[string]float64, len(points))
			for _, lp := range points {
				key := metrics.SeriesKey(lp.Labels, state.seriesLabel)
				values[key] = lp.Point.V
			}
		}
	}
	if len(values) == 0 {
		values = map[string]float64{"value": 0}
	}

	content := pushward.Content{
		Template:    templateTimeline,
		Value:       any(values),
		Subtitle:    "Grafana",
		Icon:        resolvedIcon,
		AccentColor: pushward.ColorGreen,
		State:       "Resolved",
		Scale:       h.cfg.Scale,
		Smoothing:   h.cfg.Smoothing,
		Decimals:    h.cfg.Decimals,
	}

	err := h.pwClient.UpdateActivity(ctx, state.slug, pushward.UpdateRequest{
		State:   pushward.StateEnded,
		Content: content,
	})
	if err != nil {
		logger.Error("failed to end resolved activity", "error", err)
		return
	}
	logger.Info("activity ended (alert no longer firing)")
}

// WaitIdle blocks until all in-flight async webhook goroutines complete.
func (h *Handler) WaitIdle() { h.wg.Wait() }

func (h *Handler) activeAlerts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	names := make([]string, 0, len(h.active))
	for name := range h.active {
		names = append(names, name)
	}
	return names
}

func parseAnnotationThreshold(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "> <!=")
	s = strings.TrimSpace(s)
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}
