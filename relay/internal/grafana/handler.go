package grafana

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

type Handler struct {
	clients *client.Pool
	config  *config.GrafanaConfig
}

func NewHandler(clients *client.Pool, cfg *config.GrafanaConfig) *Handler {
	return &Handler{
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

func isKnownSeverity(s string) bool {
	return s == pushward.SeverityCritical || s == pushward.SeverityWarning || s == pushward.SeverityInfo
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
	cl := h.clients.Get(userKey)

	var apiErr error
	for _, a := range payload.Alerts {
		severity := a.Labels[h.config.SeverityLabel]
		if !isKnownSeverity(severity) {
			severity = h.config.DefaultSeverity
		}

		alertname := a.Labels["alertname"]
		if alertname == "" {
			alertname = "Grafana Alert"
		}

		summary := a.Annotations["summary"]

		subtitle := "Grafana"
		if instance := a.Labels["instance"]; instance != "" {
			subtitle = "Grafana · " + instance
		}

		req := pushward.SendNotificationRequest{
			Title:      alertname,
			Subtitle:   text.Truncate(subtitle, 80),
			ThreadID:   "grafana",
			CollapseID: text.SlugHash("grafana", alertname+":"+a.Fingerprint, 6),
			Source:     "grafana",
			Push:       true,
		}

		switch a.Status {
		case "firing":
			req.Body = text.Truncate(summary, 120)
			req.Level = pushward.LevelActive
			req.Category = severity
		case "resolved":
			req.Body = "Resolved"
			if summary != "" {
				req.Body = "Resolved · " + text.Truncate(summary, 100)
			}
			req.Level = pushward.LevelPassive
			req.Category = "resolved"
		default:
			log.Warn("unknown alert status", "status", a.Status, "fingerprint", a.Fingerprint)
			continue
		}

		if err := cl.SendNotification(ctx, req); err != nil {
			log.Error("failed to send notification", "alertname", alertname, "error", err)
			apiErr = err
		} else {
			log.Info("notification sent", "alertname", alertname, "severity", severity, "status", a.Status)
		}
	}

	if apiErr != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
