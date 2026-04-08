package grafana

import (
	"context"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// Handler processes Grafana alerting webhooks.
type Handler struct {
	clients *client.Pool
	config  *config.GrafanaConfig
}

// RegisterRoutes registers the Grafana webhook endpoint with the Huma API.
func RegisterRoutes(api huma.API, clients *client.Pool, cfg *config.GrafanaConfig) {
	h := &Handler{clients: clients, config: cfg}
	humautil.RegisterWebhook(api, "/grafana", "post-grafana-webhook",
		"Receive Grafana alert webhook",
		"Processes Grafana alerting webhook payloads and sends push notifications for each alert.",
		[]string{"Grafana"}, h.handleWebhook)
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
	SilenceURL   string            `json:"silenceURL"`
	Values       map[string]any    `json:"values"`
	ValueString  string            `json:"valueString"`
	ImageURL     string            `json:"imageURL"`
}

func isKnownSeverity(s string) bool {
	return s == pushward.SeverityCritical || s == pushward.SeverityWarning || s == pushward.SeverityInfo
}

func (h *Handler) handleWebhook(ctx context.Context, input *struct {
	Body webhookPayload
}) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, "grafana")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	cl := h.clients.Get(userKey)

	var apiErr error
	for _, a := range input.Body.Alerts {
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

		// URL: pick first non-empty dashboard/panel/generator URL
		switch {
		case a.DashboardURL != "":
			req.URL = a.DashboardURL
		case a.PanelURL != "":
			req.URL = a.PanelURL
		case a.GeneratorURL != "":
			req.URL = a.GeneratorURL
		}

		// Image: panel screenshot (requires Grafana Image Renderer)
		if a.ImageURL != "" {
			req.ImageURL = a.ImageURL
		}

		// Metadata: curated alert context (server enforces max 20 keys, 512-char values).
		meta := make(map[string]string, 20)
		addMeta := func(k, v string) {
			if len(meta) >= 20 || v == "" {
				return
			}
			meta[k] = text.TruncateHard(v, 512)
		}
		// High-value labels first.
		for _, key := range []string{"alertname", "severity", "instance", "job", "namespace", "cluster", "pod", "container", "service"} {
			addMeta(key, a.Labels[key])
		}
		// All annotations (prefixed to avoid collision with labels).
		for k, v := range a.Annotations {
			addMeta("annotation_"+k, v)
		}
		// Alert metadata.
		addMeta("starts_at", a.StartsAt)
		addMeta("silence_url", a.SilenceURL)
		addMeta("generator_url", a.GeneratorURL)
		addMeta("values", a.ValueString)
		if len(meta) > 0 {
			req.Metadata = meta
		}

		if err := cl.SendNotification(ctx, req); err != nil {
			log.Error("failed to send notification", "alertname", alertname, "error", err)
			apiErr = err
		} else {
			log.Info("notification sent", "alertname", alertname, "severity", severity, "status", a.Status)
		}
	}

	if apiErr != nil {
		return nil, huma.Error502BadGateway("upstream API error")
	}
	return humautil.NewOK(), nil
}
