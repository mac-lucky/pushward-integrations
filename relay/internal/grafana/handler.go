package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

var severityRank = map[string]int{
	"critical": 3,
	"warning":  2,
	"info":     1,
}

type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.GrafanaConfig
}

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
	return text.SlugHash("grafana", alertname, 6)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var payload webhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		slog.Error("failed to decode webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	ctx = metrics.WithProvider(ctx, "grafana")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	pwClient := h.clients.Get(userKey)

	var apiErr error
	for _, a := range payload.Alerts {
		severity := a.Labels[h.config.SeverityLabel]
		if _, ok := severityRank[severity]; !ok {
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

		secondaryURL := text.SanitizeURL(a.PanelURL)
		if secondaryURL == "" {
			secondaryURL = text.SanitizeURL(a.DashboardURL)
		}

		info := &instanceInfo{
			Severity:     severity,
			Summary:      summary,
			Subtitle:     subtitle,
			FiredAt:      firedAt,
			GeneratorURL: text.SanitizeURL(a.GeneratorURL),
			SecondaryURL: secondaryURL,
		}

		switch a.Status {
		case "firing":
			if err := h.handleFiring(ctx, userKey, log, pwClient, alertname, a.Fingerprint, info); err != nil {
				apiErr = err
			}
		case "resolved":
			if err := h.handleResolved(ctx, userKey, log, pwClient, alertname, a.Fingerprint, info); err != nil {
				apiErr = err
			}
		default:
			log.Warn("unknown alert status", "status", a.Status, "fingerprint", a.Fingerprint)
		}
	}

	if apiErr != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleFiring(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, alertname, fingerprint string, info *instanceInfo) error {
	existing, err := h.store.GetGroup(ctx, "grafana", userKey, alertname)
	if err != nil {
		log.Error("failed to get alert group", "alertname", alertname, "error", err)
		return nil
	}
	isNew := len(existing) == 0

	data, _ := json.Marshal(info)
	if err := h.store.Set(ctx, "grafana", userKey, alertname, fingerprint, data, h.config.StaleTimeout); err != nil {
		log.Error("failed to store instance", "alertname", alertname, "fingerprint", fingerprint, "error", err)
		return nil
	}

	slug := slugForAlertname(alertname)

	if isNew {
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pwClient.CreateActivity(ctx, slug, alertname, h.config.Priority, endedTTL, staleTTL); err != nil {
			log.Error("failed to create activity", "slug", slug, "error", err)
			if err := h.store.DeleteGroup(ctx, "grafana", userKey, alertname); err != nil {
				log.Warn("state store delete group failed", "error", err, "provider", "grafana", "alertname", alertname)
			}
			return err
		}
		log.Info("created activity", "slug", slug, "alertname", alertname)
	}

	// Merge newly written instance into the first read to avoid a second DB round-trip.
	existing[fingerprint] = json.RawMessage(data)

	req := h.buildOngoingUpdate(existing)
	if err := pwClient.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update activity", "slug", slug, "error", err)
		return err
	}
	log.Info("updated activity", "slug", slug, "state", pushward.StateOngoing, "severity", req.Content.Severity)
	return nil
}

func (h *Handler) handleResolved(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, alertname, fingerprint string, info *instanceInfo) error {
	existing, err := h.store.Get(ctx, "grafana", userKey, alertname, fingerprint)
	if err != nil {
		log.Error("failed to check instance", "alertname", alertname, "fingerprint", fingerprint, "error", err)
		return nil
	}
	if existing == nil {
		return nil
	}

	if err := h.store.Delete(ctx, "grafana", userKey, alertname, fingerprint); err != nil {
		log.Error("failed to delete instance", "alertname", alertname, "fingerprint", fingerprint, "error", err)
		return nil
	}

	remaining, err := h.store.GetGroup(ctx, "grafana", userKey, alertname)
	if err != nil {
		log.Error("failed to get remaining instances", "alertname", alertname, "error", err)
		return nil
	}

	slug := slugForAlertname(alertname)

	var req pushward.UpdateRequest
	if len(remaining) == 0 {
		req = buildEndUpdate(info)
	} else {
		req = h.buildOngoingUpdate(remaining)
	}

	if err := pwClient.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update activity on resolve", "slug", slug, "error", err)
		return err
	}

	if len(remaining) == 0 {
		log.Info("ended activity", "slug", slug, "state", pushward.StateEnded)
		if err := h.store.DeleteGroup(ctx, "grafana", userKey, alertname); err != nil {
			log.Warn("state store delete group failed", "error", err, "provider", "grafana", "alertname", alertname)
		}
	} else {
		log.Info("updated activity after partial resolve", "slug", slug, "remaining", len(remaining))
	}
	return nil
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

	if len(decoded) == 0 {
		return pushward.UpdateRequest{State: pushward.StateOngoing, Content: pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       "Alert firing",
			Icon:        h.iconForSeverity(h.config.DefaultSeverity),
			Subtitle:    "Grafana",
			AccentColor: h.colorForSeverity(h.config.DefaultSeverity),
			Severity:    h.config.DefaultSeverity,
		}}
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

	var firedAtPtr *int64
	if worst.FiredAt > 0 {
		firedAtPtr = pushward.Int64Ptr(worst.FiredAt)
	}

	return pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:     "alert",
			Progress:     1.0,
			State:        stateText,
			Icon:         h.iconForSeverity(worst.Severity),
			Subtitle:     subtitle,
			AccentColor:  h.colorForSeverity(worst.Severity),
			Severity:     worst.Severity,
			FiredAt:      firedAtPtr,
			URL:          worst.GeneratorURL,
			SecondaryURL: worst.SecondaryURL,
		},
	}
}

func buildEndUpdate(info *instanceInfo) pushward.UpdateRequest {
	var firedAtPtr *int64
	if info.FiredAt > 0 {
		firedAtPtr = pushward.Int64Ptr(info.FiredAt)
	}
	return pushward.UpdateRequest{
		State: pushward.StateEnded,
		Content: pushward.Content{
			Template:     "alert",
			Progress:     1.0,
			State:        info.Summary,
			Icon:         "checkmark.circle.fill",
			Subtitle:     info.Subtitle,
			AccentColor:  pushward.ColorGreen,
			Severity:     info.Severity,
			FiredAt:      firedAtPtr,
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
		return pushward.ColorRed
	case "warning":
		return pushward.ColorOrange
	case "info":
		return pushward.ColorBlue
	default:
		return pushward.ColorOrange
	}
}
