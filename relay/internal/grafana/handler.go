package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// Handler processes Grafana alerting webhooks.
type Handler struct {
	clients *client.Pool
	config  *config.GrafanaConfig
	store   state.Store
}

// RegisterRoutes registers the Grafana webhook endpoint with the Huma API.
func RegisterRoutes(api huma.API, store state.Store, clients *client.Pool, cfg *config.GrafanaConfig) {
	h := &Handler{clients: clients, config: cfg, store: store}
	humautil.RegisterWebhook(api, "/grafana", "post-grafana-webhook",
		"Receive Grafana alert webhook",
		"Processes Grafana alerting webhook payloads and sends push notifications for each alert.",
		[]string{"Grafana"}, h.handleWebhook)
}

type grafanaPayload struct {
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

// alertGroup collects alerts sharing the same alertname within a single webhook payload.
type alertGroup struct {
	alertname string
	firing    []alert
	resolved  []alert
}

// alertGroupState is the JSON-serialized state stored for dedup.
type alertGroupState struct {
	Firing   []string `json:"firing"`
	Resolved []string `json:"resolved"`
}

func (s *alertGroupState) equals(other *alertGroupState) bool {
	if other == nil {
		return false
	}
	a := slices.Clone(s.Firing)
	b := slices.Clone(other.Firing)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		return false
	}
	a = slices.Clone(s.Resolved)
	b = slices.Clone(other.Resolved)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

var severityRank = map[string]int{
	pushward.SeverityCritical: 3,
	pushward.SeverityWarning:  2,
	pushward.SeverityInfo:     1,
}

func isKnownSeverity(s string) bool {
	return s == pushward.SeverityCritical || s == pushward.SeverityWarning || s == pushward.SeverityInfo
}

// highestSeverity returns the most critical severity from the list, falling back to defaultSev.
func highestSeverity(severities []string, defaultSev string) string {
	best := ""
	bestRank := 0
	for _, s := range severities {
		if r, ok := severityRank[s]; ok && r > bestRank {
			best = s
			bestRank = r
		}
	}
	if best == "" {
		return defaultSev
	}
	return best
}

// formatGroupSubtitle builds subtitle like "Grafana · 3 firing, 1 resolved".
func formatGroupSubtitle(firing, resolved int) string {
	var parts []string
	if firing > 0 {
		parts = append(parts, fmt.Sprintf("%d firing", firing))
	}
	if resolved > 0 {
		parts = append(parts, fmt.Sprintf("%d resolved", resolved))
	}
	if len(parts) == 0 {
		return "Grafana"
	}
	return "Grafana · " + strings.Join(parts, ", ")
}

func (h *Handler) handleWebhook(ctx context.Context, input *struct {
	Body grafanaPayload
}) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, "grafana")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	cl := h.clients.Get(userKey)

	// Group alerts by alertname, preserving insertion order.
	groups, groupOrder := h.groupAlerts(log, input.Body.Alerts)

	var apiErr error
	for _, alertname := range groupOrder {
		g := groups[alertname]

		// Check state for dedup.
		stateKey := text.Slug("", alertname)
		currentState := h.buildState(g)
		if h.stateUnchanged(ctx, userKey, stateKey, currentState, log) {
			continue
		}

		// Build and send notification.
		var req pushward.SendNotificationRequest
		if len(g.firing)+len(g.resolved) == 1 {
			req = h.buildSingleNotification(g)
		} else {
			req = h.buildGroupedNotification(g)
		}

		if err := cl.SendNotification(ctx, req); err != nil {
			log.Error("failed to send notification", "alertname", alertname, "error", err)
			apiErr = err
			continue
		}
		log.Info("notification sent", "alertname", alertname,
			"firing", len(g.firing), "resolved", len(g.resolved))

		// Store state after successful send.
		h.updateState(ctx, userKey, stateKey, g, currentState, log)
	}

	if apiErr != nil {
		return nil, huma.Error502BadGateway("upstream API error")
	}
	return humautil.NewOK(), nil
}

// groupAlerts partitions alerts by alertname, skipping unknown statuses.
func (h *Handler) groupAlerts(log *slog.Logger, alerts []alert) (map[string]*alertGroup, []string) {
	groups := make(map[string]*alertGroup)
	var order []string

	for _, a := range alerts {
		if a.Status != "firing" && a.Status != "resolved" {
			log.Warn("unknown alert status", "status", a.Status, "fingerprint", a.Fingerprint)
			continue
		}

		alertname := a.Labels["alertname"]
		if alertname == "" {
			alertname = "Grafana Alert"
		}

		g, exists := groups[alertname]
		if !exists {
			g = &alertGroup{alertname: alertname}
			groups[alertname] = g
			order = append(order, alertname)
		}

		if a.Status == "firing" {
			g.firing = append(g.firing, a)
		} else {
			g.resolved = append(g.resolved, a)
		}
	}
	return groups, order
}

// buildState creates the fingerprint-set state for a group.
func (h *Handler) buildState(g *alertGroup) *alertGroupState {
	s := &alertGroupState{
		Firing:   make([]string, 0, len(g.firing)),
		Resolved: make([]string, 0, len(g.resolved)),
	}
	for _, a := range g.firing {
		s.Firing = append(s.Firing, a.Fingerprint)
	}
	for _, a := range g.resolved {
		s.Resolved = append(s.Resolved, a.Fingerprint)
	}
	return s
}

// stateUnchanged checks if the alert group state matches the previously stored state.
func (h *Handler) stateUnchanged(ctx context.Context, userKey, stateKey string, current *alertGroupState, log *slog.Logger) bool {
	raw, err := h.store.Get(ctx, "grafana", userKey, stateKey, "")
	if err != nil {
		log.Warn("failed to read alert state", "key", stateKey, "error", err)
		return false // on error, treat as changed to avoid dropping notifications
	}
	if raw == nil {
		return false // no prior state
	}

	var prev alertGroupState
	if err := json.Unmarshal(raw, &prev); err != nil {
		log.Warn("failed to decode alert state", "key", stateKey, "error", err)
		return false
	}
	if current.equals(&prev) {
		log.Debug("alert state unchanged, skipping", "alertname", stateKey)
		return true
	}
	return false
}

// updateState stores or deletes state after a successful notification send.
func (h *Handler) updateState(ctx context.Context, userKey, stateKey string, g *alertGroup, current *alertGroupState, log *slog.Logger) {
	if len(g.firing) == 0 {
		// All resolved — delete state.
		if err := h.store.Delete(ctx, "grafana", userKey, stateKey, ""); err != nil {
			log.Warn("failed to delete alert state", "key", stateKey, "error", err)
		}
		return
	}

	data, err := json.Marshal(current)
	if err != nil {
		log.Warn("failed to marshal alert state", "key", stateKey, "error", err)
		return
	}
	if err := h.store.Set(ctx, "grafana", userKey, stateKey, "", data, h.config.StaleTimeout); err != nil {
		log.Warn("failed to store alert state", "key", stateKey, "error", err)
	}
}

// buildSingleNotification constructs a notification for a group with exactly one alert.
// This preserves the original per-alert behavior (instance in subtitle, per-fingerprint collapse ID).
func (h *Handler) buildSingleNotification(g *alertGroup) pushward.SendNotificationRequest {
	var a alert
	if len(g.firing) > 0 {
		a = g.firing[0]
	} else {
		a = g.resolved[0]
	}

	severity := a.Labels[h.config.SeverityLabel]
	if !isKnownSeverity(severity) {
		severity = h.config.DefaultSeverity
	}

	summary := a.Annotations["summary"]

	subtitle := "Grafana"
	if instance := a.Labels["instance"]; instance != "" {
		subtitle = "Grafana · " + instance
	}

	req := pushward.SendNotificationRequest{
		Title:      g.alertname,
		Subtitle:   text.Truncate(subtitle, 80),
		ThreadID:   text.Slug("grafana-", g.alertname),
		CollapseID: text.SlugHash("grafana", g.alertname+":"+a.Fingerprint, 6),
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
	}

	h.setURL(&req, a)
	if a.ImageURL != "" {
		req.ImageURL = a.ImageURL
	}
	req.Metadata = h.buildAlertMetadata(a)
	return req
}

// buildGroupedNotification constructs a notification for a group with 2+ alerts.
func (h *Handler) buildGroupedNotification(g *alertGroup) pushward.SendNotificationRequest {
	// Pick the representative alert (first firing, or first resolved if none firing).
	var representative alert
	if len(g.firing) > 0 {
		representative = g.firing[0]
	} else {
		representative = g.resolved[0]
	}

	subtitle := formatGroupSubtitle(len(g.firing), len(g.resolved))

	req := pushward.SendNotificationRequest{
		Title:      g.alertname,
		Subtitle:   text.Truncate(subtitle, 80),
		ThreadID:   text.Slug("grafana-", g.alertname),
		CollapseID: text.SlugHash("grafana", g.alertname, 6),
		Source:     "grafana",
		Push:       true,
	}

	if len(g.firing) > 0 {
		// Find highest severity among firing alerts.
		severities := make([]string, 0, len(g.firing))
		for _, a := range g.firing {
			sev := a.Labels[h.config.SeverityLabel]
			if !isKnownSeverity(sev) {
				sev = h.config.DefaultSeverity
			}
			severities = append(severities, sev)
		}
		req.Category = highestSeverity(severities, h.config.DefaultSeverity)
		req.Level = pushward.LevelActive

		// Body: first firing alert's summary.
		summary := h.firstNonEmptySummary(g.firing)
		if summary != "" {
			req.Body = text.Truncate(summary, 120)
		} else {
			req.Body = fmt.Sprintf("%d alerts firing", len(g.firing))
		}
	} else {
		req.Category = "resolved"
		req.Level = pushward.LevelPassive

		summary := h.firstNonEmptySummary(g.resolved)
		if summary != "" {
			req.Body = "Resolved · " + text.Truncate(summary, 100)
		} else {
			req.Body = fmt.Sprintf("%d alerts resolved", len(g.resolved))
		}
	}

	h.setURL(&req, representative)
	if representative.ImageURL != "" {
		req.ImageURL = representative.ImageURL
	}
	req.Metadata = h.buildGroupedMetadata(g, representative)
	return req
}

// firstNonEmptySummary returns the first non-empty summary annotation from the given alerts.
func (h *Handler) firstNonEmptySummary(alerts []alert) string {
	for _, a := range alerts {
		if s := a.Annotations["summary"]; s != "" {
			return s
		}
	}
	return ""
}

// setURL picks the first non-empty dashboard/panel/generator URL.
func (h *Handler) setURL(req *pushward.SendNotificationRequest, a alert) {
	switch {
	case a.DashboardURL != "":
		req.URL = a.DashboardURL
	case a.PanelURL != "":
		req.URL = a.PanelURL
	case a.GeneratorURL != "":
		req.URL = a.GeneratorURL
	}
}

// buildAlertMetadata builds metadata for a single-alert notification.
func (h *Handler) buildAlertMetadata(a alert) map[string]string {
	meta := make(map[string]string, 20)
	addMeta := func(k, v string) {
		if len(meta) >= 20 || v == "" {
			return
		}
		meta[k] = text.TruncateHard(v, 512)
	}
	for _, key := range []string{"alertname", "severity", "instance", "job", "job_name", "namespace", "cluster", "pod", "container", "service"} {
		addMeta(key, a.Labels[key])
	}
	addMeta("fingerprint", a.Fingerprint)
	for k, v := range a.Annotations {
		addMeta("annotation_"+k, v)
	}
	addMeta("starts_at", a.StartsAt)
	addMeta("silence_url", a.SilenceURL)
	addMeta("generator_url", a.GeneratorURL)
	addMeta("values", a.ValueString)
	if len(meta) > 0 {
		return meta
	}
	return nil
}

// buildGroupedMetadata builds metadata for a grouped notification.
func (h *Handler) buildGroupedMetadata(g *alertGroup, representative alert) map[string]string {
	meta := make(map[string]string, 20)
	addMeta := func(k, v string) {
		if len(meta) >= 20 || v == "" {
			return
		}
		meta[k] = text.TruncateHard(v, 512)
	}

	// Counts.
	addMeta("firing_count", strconv.Itoa(len(g.firing)))
	addMeta("resolved_count", strconv.Itoa(len(g.resolved)))

	// Instance lists.
	addMeta("firing_instances", collectInstances(g.firing))
	addMeta("resolved_instances", collectInstances(g.resolved))

	// Representative alert labels.
	for _, key := range []string{"alertname", "severity", "job", "job_name", "namespace", "cluster"} {
		addMeta(key, representative.Labels[key])
	}
	for k, v := range representative.Annotations {
		addMeta("annotation_"+k, v)
	}
	addMeta("starts_at", representative.StartsAt)
	addMeta("silence_url", representative.SilenceURL)
	addMeta("generator_url", representative.GeneratorURL)
	addMeta("values", representative.ValueString)
	if len(meta) > 0 {
		return meta
	}
	return nil
}

// collectInstances returns a comma-separated list of instance labels from the alerts.
func collectInstances(alerts []alert) string {
	var instances []string
	for _, a := range alerts {
		if inst := a.Labels["instance"]; inst != "" {
			instances = append(instances, inst)
		}
	}
	return text.TruncateHard(strings.Join(instances, ", "), 512)
}
