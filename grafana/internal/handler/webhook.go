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

	templateTimeline    = "timeline"
	defaultWarningIcon  = "exclamationmark.triangle.fill"
	resolvedIcon        = "checkmark.circle.fill"
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
	slug     string
	expr     string
	refID    string
	lastSeen time.Time
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

	// Check-and-mark: reserve the slot before releasing the lock to prevent
	// duplicate creates from concurrent webhooks.
	h.mu.Lock()
	existing := h.active[alertname]
	isNew := existing == nil
	if isNew {
		h.active[alertname] = &alertState{slug: slug, lastSeen: time.Now()}
	} else {
		existing.lastSeen = time.Now()
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
	value := h.resolveValue(a, refID)
	content := h.buildContent(a, severity, value)

	if isNew && expr != "" {
		history := h.fetchHistory(ctx, logger, expr)
		if len(history) > 0 {
			content.History = map[string][]pushward.HistoryPoint{"": history}
		}
	}

	err := h.pwClient.UpdateActivity(ctx, slug, pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	})
	if err != nil {
		logger.Error("failed to update activity", "error", err)
		return
	}

	if isNew && expr != "" {
		h.poller.Start(slug, expr)
		logger.Info("poller started")
	}
}

func (h *Handler) handleResolved(ctx context.Context, a alert) {
	alertname := a.Labels["alertname"]
	if alertname == "" {
		alertname = "Grafana Alert"
	}
	slug := makeSlug(alertname)
	logger := slog.With("alertname", alertname, "slug", slug)

	// Capture refID before deleting state so resolveValue uses the correct key.
	h.mu.Lock()
	state, exists := h.active[alertname]
	var refID string
	if exists {
		refID = state.refID
	}
	delete(h.active, alertname)
	h.mu.Unlock()

	if !exists {
		return
	}

	h.poller.Stop(slug)

	severity := h.resolveSeverity(a)
	value := h.resolveValue(a, refID)
	content := h.buildContent(a, severity, value)
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

func (h *Handler) resolveValue(a alert, preferredRefID string) *float64 {
	if len(a.Values) == 0 {
		return nil
	}

	if rid := a.Annotations["pushward_ref_id"]; rid != "" {
		preferredRefID = rid
	}

	if preferredRefID != "" {
		if v, ok := a.Values[preferredRefID]; ok {
			return &v
		}
	}

	for _, v := range a.Values {
		return &v
	}
	return nil
}

func (h *Handler) buildContent(a alert, severity string, value *float64) pushward.Content {
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

func (h *Handler) fetchHistory(ctx context.Context, logger *slog.Logger, expr string) []pushward.HistoryPoint {
	now := time.Now()
	step := h.cfg.HistoryWindow / 120
	if step < 15*time.Second {
		step = 15 * time.Second
	}

	points, err := h.metricsClient.QueryRange(ctx, expr, now.Add(-h.cfg.HistoryWindow), now, step)
	if err != nil {
		logger.Warn("failed to fetch history", "error", err)
		return nil
	}
	return points
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

// waitIdle blocks until all in-flight async webhook goroutines complete.
func (h *Handler) waitIdle() { h.wg.Wait() }

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
