package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-docker/grafana/internal/config"
	"github.com/mac-lucky/pushward-docker/grafana/internal/grafana"
	"github.com/mac-lucky/pushward-docker/grafana/internal/pushward"
)

type Handler struct {
	client       *pushward.Client
	config       *config.Config
	mu           sync.Mutex
	activeAlerts map[string]*activeAlert
}

type activeAlert struct {
	slug         string
	firedAt      int64
	cleanupTimer *time.Timer
	staleTimer   *time.Timer
}

func New(client *pushward.Client, cfg *config.Config) *Handler {
	return &Handler{
		client:       client,
		config:       cfg,
		activeAlerts: make(map[string]*activeAlert),
	}
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
		slug := "grafana-" + fingerprint[:min(12, len(fingerprint))]

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

		icon := h.iconForSeverity(severity)

		generatorURL := alert.GeneratorURL
		secondaryURL := alert.PanelURL
		if secondaryURL == "" {
			secondaryURL = alert.DashboardURL
		}

		switch alert.Status {
		case "firing":
			h.handleFiring(ctx, slug, alertname, summary, subtitle, icon, severity, firedAt, generatorURL, secondaryURL)
		case "resolved":
			h.handleResolved(ctx, slug, summary, subtitle, icon, severity, firedAt, generatorURL, secondaryURL)
		default:
			slog.Warn("unknown alert status", "status", alert.Status, "fingerprint", fingerprint)
		}
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleFiring(ctx context.Context, slug, alertname, summary, subtitle, icon, severity string, firedAt int64, generatorURL, secondaryURL string) {
	h.mu.Lock()
	existing := h.activeAlerts[slug]
	h.mu.Unlock()

	if existing == nil {
		if err := h.client.CreateActivity(ctx, slug, alertname, h.config.PushWard.Priority); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			return
		}
		slog.Info("created activity", "slug", slug, "alertname", alertname)
	}

	firedAtPtr := &firedAt
	req := pushward.UpdateRequest{
		State: "ONGOING",
		Content: pushward.Content{
			Template:     "alert",
			Progress:     1.0,
			State:        summary,
			Icon:         icon,
			Subtitle:     subtitle,
			AccentColor:  h.colorForSeverity(severity),
			Severity:     severity,
			FiredAt:      firedAtPtr,
			URL:          generatorURL,
			SecondaryURL: secondaryURL,
		},
	}

	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "state", "ONGOING", "severity", severity)

	h.mu.Lock()
	if existing != nil {
		// Reset stale timer on re-fire
		if existing.staleTimer != nil {
			existing.staleTimer.Stop()
		}
		existing.staleTimer = time.AfterFunc(h.config.PushWard.StaleTimeout, func() {
			h.forceEnd(slug)
		})
		// Cancel any pending cleanup from a previous resolved state
		if existing.cleanupTimer != nil {
			existing.cleanupTimer.Stop()
			existing.cleanupTimer = nil
		}
	} else {
		aa := &activeAlert{
			slug:    slug,
			firedAt: firedAt,
			staleTimer: time.AfterFunc(h.config.PushWard.StaleTimeout, func() {
				h.forceEnd(slug)
			}),
		}
		h.activeAlerts[slug] = aa
	}
	h.mu.Unlock()
}

func (h *Handler) handleResolved(ctx context.Context, slug, summary, subtitle, icon, severity string, firedAt int64, generatorURL, secondaryURL string) {
	firedAtPtr := &firedAt
	req := pushward.UpdateRequest{
		State: "ENDED",
		Content: pushward.Content{
			Template:     "alert",
			Progress:     1.0,
			State:        summary,
			Icon:         "checkmark.circle.fill",
			Subtitle:     subtitle,
			AccentColor:  "#34C759",
			Severity:     severity,
			FiredAt:      firedAtPtr,
			URL:          generatorURL,
			SecondaryURL: secondaryURL,
		},
	}

	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to end activity", "slug", slug, "error", err)
		return
	}
	slog.Info("ended activity", "slug", slug, "state", "ENDED")

	h.mu.Lock()
	if aa, ok := h.activeAlerts[slug]; ok {
		if aa.staleTimer != nil {
			aa.staleTimer.Stop()
		}
		aa.cleanupTimer = time.AfterFunc(h.config.PushWard.CleanupDelay, func() {
			h.cleanup(slug)
		})
	}
	h.mu.Unlock()
}

func (h *Handler) forceEnd(slug string) {
	slog.Warn("force-ending stale alert", "slug", slug)
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
		h.cleanup(slug)
	})
}

func (h *Handler) cleanup(slug string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := h.client.DeleteActivity(ctx, slug); err != nil {
		slog.Error("failed to delete activity", "slug", slug, "error", err)
		return
	}
	slog.Info("deleted activity", "slug", slug)

	h.mu.Lock()
	delete(h.activeAlerts, slug)
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
