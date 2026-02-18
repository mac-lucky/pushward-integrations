package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-docker/grafana/internal/config"
	"github.com/mac-lucky/pushward-docker/grafana/internal/grafana"
	"github.com/mac-lucky/pushward-docker/grafana/internal/pushward"
)

type Handler struct {
	client      *pushward.Client
	config      *config.Config
	mu          sync.Mutex
	alertGroups map[string]*alertGroup // alertname -> group
}

// alertGroup tracks all firing instances of a single alert rule as one activity.
type alertGroup struct {
	slug         string
	alertname    string
	instances    map[string]*instanceInfo // fingerprint -> info
	cleanupTimer *time.Timer
	staleTimer   *time.Timer
}

type instanceInfo struct {
	severity     string
	summary      string
	subtitle     string
	firedAt      int64
	generatorURL string
	secondaryURL string
}

func New(client *pushward.Client, cfg *config.Config) *Handler {
	return &Handler{
		client:      client,
		config:      cfg,
		alertGroups: make(map[string]*alertGroup),
	}
}

// slugForAlertname derives a stable, URL-safe activity slug from an alert rule name.
func slugForAlertname(alertname string) string {
	h := sha256.Sum256([]byte(alertname))
	return fmt.Sprintf("grafana-%x", h[:6])
}

func (h *Handler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload grafana.WebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		slog.Error("failed to decode webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	for _, alert := range payload.Alerts {
		fingerprint := alert.Fingerprint

		severity := alert.Labels[h.config.Grafana.SeverityLabel]
		if severity == "" {
			severity = h.config.Grafana.DefaultSeverity
		}

		alertname := alert.Labels["alertname"]
		if alertname == "" {
			alertname = "Grafana Alert"
		}

		summary := alert.Annotations["summary"]

		subtitle := "Grafana"
		if instance := alert.Labels["instance"]; instance != "" {
			subtitle = "Grafana \u00b7 " + instance
		}

		var firedAt int64
		if t, err := time.Parse(time.RFC3339, alert.StartsAt); err == nil {
			firedAt = t.Unix()
		}

		secondaryURL := alert.PanelURL
		if secondaryURL == "" {
			secondaryURL = alert.DashboardURL
		}

		info := &instanceInfo{
			severity:     severity,
			summary:      summary,
			subtitle:     subtitle,
			firedAt:      firedAt,
			generatorURL: alert.GeneratorURL,
			secondaryURL: secondaryURL,
		}

		switch alert.Status {
		case "firing":
			h.handleFiring(ctx, alertname, fingerprint, info)
		case "resolved":
			h.handleResolved(ctx, alertname, fingerprint, info)
		default:
			slog.Warn("unknown alert status", "status", alert.Status, "fingerprint", fingerprint)
		}
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleFiring(ctx context.Context, alertname, fingerprint string, info *instanceInfo) {
	slug := slugForAlertname(alertname)

	h.mu.Lock()
	group, exists := h.alertGroups[alertname]
	isNew := !exists
	if isNew {
		group = &alertGroup{
			slug:      slug,
			alertname: alertname,
			instances: make(map[string]*instanceInfo),
		}
		h.alertGroups[alertname] = group
	}
	group.instances[fingerprint] = info
	if group.cleanupTimer != nil {
		group.cleanupTimer.Stop()
		group.cleanupTimer = nil
	}
	if group.staleTimer != nil {
		group.staleTimer.Stop()
		group.staleTimer = nil
	}
	h.mu.Unlock()

	if isNew {
		if err := h.client.CreateActivity(ctx, slug, alertname, h.config.PushWard.Priority); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			delete(h.alertGroups, alertname)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity", "slug", slug, "alertname", alertname)
	}

	h.mu.Lock()
	req := h.buildOngoingUpdate(group)
	h.mu.Unlock()

	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "state", "ONGOING", "severity", req.Content.Severity)

	h.mu.Lock()
	if g, ok := h.alertGroups[alertname]; ok {
		g.staleTimer = time.AfterFunc(h.config.PushWard.StaleTimeout, func() {
			h.forceEnd(alertname)
		})
	}
	h.mu.Unlock()
}

func (h *Handler) handleResolved(ctx context.Context, alertname, fingerprint string, info *instanceInfo) {
	h.mu.Lock()
	group, exists := h.alertGroups[alertname]
	if !exists {
		h.mu.Unlock()
		return
	}
	if _, tracked := group.instances[fingerprint]; !tracked {
		h.mu.Unlock()
		return
	}
	delete(group.instances, fingerprint)
	remaining := len(group.instances)
	slug := group.slug

	var req pushward.UpdateRequest
	if remaining == 0 {
		if group.staleTimer != nil {
			group.staleTimer.Stop()
			group.staleTimer = nil
		}
		req = buildEndUpdate(info)
	} else {
		req = h.buildOngoingUpdate(group)
	}
	h.mu.Unlock()

	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity on resolve", "slug", slug, "error", err)
		return
	}

	if remaining == 0 {
		slog.Info("ended activity", "slug", slug, "state", "ENDED")
		h.mu.Lock()
		if g, ok := h.alertGroups[alertname]; ok {
			g.cleanupTimer = time.AfterFunc(h.config.PushWard.CleanupDelay, func() {
				h.cleanup(alertname)
			})
		}
		h.mu.Unlock()
	} else {
		slog.Info("updated activity after partial resolve", "slug", slug, "remaining", remaining)
	}
}

// buildOngoingUpdate constructs an ONGOING update from the group's active instances.
// Must be called with mu held.
func (h *Handler) buildOngoingUpdate(group *alertGroup) pushward.UpdateRequest {
	worst := h.worstInstance(group)
	count := len(group.instances)

	var state, subtitle string
	if count == 1 {
		state = worst.summary
		subtitle = worst.subtitle
	} else {
		state = fmt.Sprintf("%d instances firing", count)
		subtitle = "Grafana"
	}

	firedAtPtr := &worst.firedAt

	return pushward.UpdateRequest{
		State: "ONGOING",
		Content: pushward.Content{
			Template:     "alert",
			Progress:     1.0,
			State:        state,
			Icon:         h.iconForSeverity(worst.severity),
			Subtitle:     subtitle,
			AccentColor:  h.colorForSeverity(worst.severity),
			Severity:     worst.severity,
			FiredAt:      firedAtPtr,
			URL:          worst.generatorURL,
			SecondaryURL: worst.secondaryURL,
		},
	}
}

func buildEndUpdate(info *instanceInfo) pushward.UpdateRequest {
	firedAtPtr := &info.firedAt
	return pushward.UpdateRequest{
		State: "ENDED",
		Content: pushward.Content{
			Template:     "alert",
			Progress:     1.0,
			State:        info.summary,
			Icon:         "checkmark.circle.fill",
			Subtitle:     info.subtitle,
			AccentColor:  "#34C759",
			Severity:     info.severity,
			FiredAt:      firedAtPtr,
			URL:          info.generatorURL,
			SecondaryURL: info.secondaryURL,
		},
	}
}

// worstInstance returns the instance with the highest severity.
// Must be called with mu held.
func (h *Handler) worstInstance(group *alertGroup) *instanceInfo {
	severityRank := map[string]int{
		"critical": 3,
		"warning":  2,
		"info":     1,
	}
	var worst *instanceInfo
	worstRank := -1
	for _, inst := range group.instances {
		rank := severityRank[inst.severity]
		if worst == nil || rank > worstRank {
			worst = inst
			worstRank = rank
		}
	}
	return worst
}

func (h *Handler) forceEnd(alertname string) {
	h.mu.Lock()
	group, ok := h.alertGroups[alertname]
	if !ok {
		h.mu.Unlock()
		return
	}
	slug := group.slug
	h.mu.Unlock()

	slog.Warn("force-ending stale alert", "slug", slug, "alertname", alertname)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := pushward.UpdateRequest{
		State: "ENDED",
		Content: pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       "Stale alert (auto-ended)",
			Icon:        "clock.badge.xmark",
			AccentColor: "#8E8E93",
		},
	}
	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to force-end activity", "slug", slug, "error", err)
	}

	time.AfterFunc(h.config.PushWard.CleanupDelay, func() {
		h.cleanup(alertname)
	})
}

func (h *Handler) cleanup(alertname string) {
	h.mu.Lock()
	group, ok := h.alertGroups[alertname]
	if !ok {
		h.mu.Unlock()
		return
	}
	slug := group.slug
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := h.client.DeleteActivity(ctx, slug); err != nil {
		slog.Error("failed to delete activity", "slug", slug, "error", err)
		return
	}
	slog.Info("deleted activity", "slug", slug)

	h.mu.Lock()
	delete(h.alertGroups, alertname)
	h.mu.Unlock()
}

func (h *Handler) iconForSeverity(severity string) string {
	switch severity {
	case "critical":
		return "exclamationmark.octagon.fill"
	case "warning":
		return h.config.Grafana.DefaultIcon
	case "info":
		return "info.circle.fill"
	default:
		return h.config.Grafana.DefaultIcon
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
