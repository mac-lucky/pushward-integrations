package grafana

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

var severityRank = map[string]int{
	"critical": 3,
	"warning":  2,
	"info":     1,
}

// Handler processes Grafana alert webhooks for multiple tenants.
type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.GrafanaConfig
}

// NewHandler creates a new Grafana webhook handler.
func NewHandler(store state.Store, clients *client.Pool, cfg *config.GrafanaConfig) *Handler {
	return &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
	}
}

type webhookPayload struct {
	Alerts []alert `json:"alerts"`
}

type alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	Fingerprint  string            `json:"fingerprint"`
	GeneratorURL string            `json:"generatorURL"`
	DashboardURL string            `json:"dashboardURL"`
	PanelURL     string            `json:"panelURL"`
}

// instanceInfo is stored as JSON in the state store.
type instanceInfo struct {
	Severity     string `json:"severity"`
	Summary      string `json:"summary"`
	Subtitle     string `json:"subtitle"`
	FiredAt      int64  `json:"firedAt"`
	GeneratorURL string `json:"generatorURL,omitempty"`
	SecondaryURL string `json:"secondaryURL,omitempty"`
}

// slugForAlertname derives a stable, URL-safe activity slug from an alert rule name.
func slugForAlertname(alertname string) string {
	h := sha256.Sum256([]byte(alertname))
	return fmt.Sprintf("grafana-%x", h[:6])
}

// ServeHTTP handles incoming Grafana webhook requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload webhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		slog.Error("failed to decode webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	userKey := auth.KeyFromContext(ctx)
	pwClient := h.clients.Get(userKey)

	for _, a := range payload.Alerts {
		severity := a.Labels[h.config.SeverityLabel]
		if severity == "" {
			severity = h.config.DefaultSeverity
		}

		alertname := a.Labels["alertname"]
		if alertname == "" {
			alertname = "Grafana Alert"
		}

		summary := a.Annotations["summary"]

		subtitle := "Grafana"
		if instance := a.Labels["instance"]; instance != "" {
			subtitle = "Grafana \u00b7 " + instance
		}

		var firedAt int64
		if t, err := time.Parse(time.RFC3339, a.StartsAt); err == nil {
			firedAt = t.Unix()
		}

		secondaryURL := a.PanelURL
		if secondaryURL == "" {
			secondaryURL = a.DashboardURL
		}

		info := &instanceInfo{
			Severity:     severity,
			Summary:      summary,
			Subtitle:     subtitle,
			FiredAt:      firedAt,
			GeneratorURL: a.GeneratorURL,
			SecondaryURL: secondaryURL,
		}

		switch a.Status {
		case "firing":
			h.handleFiring(ctx, userKey, pwClient, alertname, a.Fingerprint, info)
		case "resolved":
			h.handleResolved(ctx, userKey, pwClient, alertname, a.Fingerprint, info)
		default:
			slog.Warn("unknown alert status", "status", a.Status, "fingerprint", a.Fingerprint)
		}
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleFiring(ctx context.Context, userKey string, pwClient *pushward.Client, alertname, fingerprint string, info *instanceInfo) {
	existing, err := h.store.GetGroup(ctx, "grafana", userKey, alertname)
	if err != nil {
		slog.Error("failed to get alert group", "alertname", alertname, "error", err)
		return
	}
	isNew := len(existing) == 0

	data, _ := json.Marshal(info)
	if err := h.store.Set(ctx, "grafana", userKey, alertname, fingerprint, data, h.config.StaleTimeout); err != nil {
		slog.Error("failed to store instance", "alertname", alertname, "fingerprint", fingerprint, "error", err)
		return
	}

	slug := slugForAlertname(alertname)

	if isNew {
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pwClient.CreateActivity(ctx, slug, alertname, h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			_ = h.store.DeleteGroup(ctx, "grafana", userKey, alertname)
			return
		}
		slog.Info("created activity", "slug", slug, "alertname", alertname)
	}

	instances, err := h.store.GetGroup(ctx, "grafana", userKey, alertname)
	if err != nil {
		slog.Error("failed to get instances for update", "alertname", alertname, "error", err)
		return
	}

	req := h.buildOngoingUpdate(instances)
	if err := pwClient.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "state", "ONGOING", "severity", req.Content.Severity)
}

func (h *Handler) handleResolved(ctx context.Context, userKey string, pwClient *pushward.Client, alertname, fingerprint string, info *instanceInfo) {
	existing, err := h.store.Get(ctx, "grafana", userKey, alertname, fingerprint)
	if err != nil {
		slog.Error("failed to check instance", "alertname", alertname, "fingerprint", fingerprint, "error", err)
		return
	}
	if existing == nil {
		return
	}

	if err := h.store.Delete(ctx, "grafana", userKey, alertname, fingerprint); err != nil {
		slog.Error("failed to delete instance", "alertname", alertname, "fingerprint", fingerprint, "error", err)
		return
	}

	remaining, err := h.store.GetGroup(ctx, "grafana", userKey, alertname)
	if err != nil {
		slog.Error("failed to get remaining instances", "alertname", alertname, "error", err)
		return
	}

	slug := slugForAlertname(alertname)

	var req pushward.UpdateRequest
	if len(remaining) == 0 {
		req = buildEndUpdate(info)
	} else {
		req = h.buildOngoingUpdate(remaining)
	}

	if err := pwClient.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity on resolve", "slug", slug, "error", err)
		return
	}

	if len(remaining) == 0 {
		slog.Info("ended activity", "slug", slug, "state", "ENDED")
		_ = h.store.DeleteGroup(ctx, "grafana", userKey, alertname)
	} else {
		slog.Info("updated activity after partial resolve", "slug", slug, "remaining", len(remaining))
	}
}

func (h *Handler) buildOngoingUpdate(instances map[string]json.RawMessage) pushward.UpdateRequest {
	decoded := make(map[string]*instanceInfo, len(instances))
	for fp, raw := range instances {
		var info instanceInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			continue
		}
		decoded[fp] = &info
	}

	worst := h.worstInstance(decoded)
	count := len(decoded)

	var stateText, subtitle string
	if count == 1 {
		stateText = worst.Summary
		subtitle = worst.Subtitle
	} else {
		stateText = fmt.Sprintf("%d instances firing", count)
		subtitle = "Grafana"
	}

	firedAt := worst.FiredAt

	return pushward.UpdateRequest{
		State: "ONGOING",
		Content: pushward.Content{
			Template:     "alert",
			Progress:     1.0,
			State:        stateText,
			Icon:         h.iconForSeverity(worst.Severity),
			Subtitle:     subtitle,
			AccentColor:  h.colorForSeverity(worst.Severity),
			Severity:     worst.Severity,
			FiredAt:      &firedAt,
			URL:          worst.GeneratorURL,
			SecondaryURL: worst.SecondaryURL,
		},
	}
}

func buildEndUpdate(info *instanceInfo) pushward.UpdateRequest {
	firedAt := info.FiredAt
	return pushward.UpdateRequest{
		State: "ENDED",
		Content: pushward.Content{
			Template:     "alert",
			Progress:     1.0,
			State:        info.Summary,
			Icon:         "checkmark.circle.fill",
			Subtitle:     info.Subtitle,
			AccentColor:  "#34C759",
			Severity:     info.Severity,
			FiredAt:      &firedAt,
			URL:          info.GeneratorURL,
			SecondaryURL: info.SecondaryURL,
		},
	}
}

func (h *Handler) worstInstance(instances map[string]*instanceInfo) *instanceInfo {
	var worst *instanceInfo
	worstRank := -1
	for _, inst := range instances {
		rank := severityRank[inst.Severity]
		if worst == nil || rank > worstRank {
			worst = inst
			worstRank = rank
		}
	}
	return worst
}

func (h *Handler) iconForSeverity(severity string) string {
	switch severity {
	case "critical":
		return "exclamationmark.octagon.fill"
	case "warning":
		return h.config.DefaultIcon
	case "info":
		return "info.circle.fill"
	default:
		return h.config.DefaultIcon
	}
}

func (h *Handler) colorForSeverity(severity string) string {
	switch severity {
	case "critical":
		return "#FF3B30"
	case "warning":
		return "#FF9500"
	case "info":
		return "#007AFF"
	default:
		return "#FF9500"
	}
}
