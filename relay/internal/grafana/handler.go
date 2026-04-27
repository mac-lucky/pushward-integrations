package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

const (
	maxMetaEntries  = 20  // PushWard API limit on metadata key-value pairs.
	maxMetaValueLen = 512 // PushWard API limit on metadata value length.
)

// metaMap is a capped metadata map that silently drops entries beyond the limit.
type metaMap map[string]string

func newMetaMap() metaMap { return make(metaMap, maxMetaEntries) }

func (m metaMap) add(k, v string) {
	if len(m) >= maxMetaEntries || v == "" {
		return
	}
	m[k] = text.TruncateHard(v, maxMetaValueLen)
}

func (m metaMap) result() map[string]string {
	if len(m) > 0 {
		return m
	}
	return nil
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
},
) (*humautil.WebhookResponse, error) {
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
		ThreadID:   "grafana",
		CollapseID: text.SlugHash("grafana", g.alertname, 6),
		Source:     "grafana",
		Push:       true,
	}

	switch a.Status {
	case "firing":
		req.Body = text.Truncate(summary, 120)
		req.Level = pushward.LevelActive
		req.Category = severity
	case "resolved":
		req.Body = "Resolved · " + g.alertname
		if summary != "" {
			req.Body = "Resolved · " + g.alertname + " · " + text.Truncate(summary, 80)
		}
		req.Level = pushward.LevelPassive
		req.Category = "resolved"
	}

	h.setURL(&req, a)
	req.Media = pushward.MediaImage(text.SanitizeURL(a.ImageURL))
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
		ThreadID:   "grafana",
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
		req.Body = formatGroupedBody(g.firing, g.alertname+" · ", "firing", 120)
	} else {
		req.Category = "resolved"
		req.Level = pushward.LevelPassive
		req.Body = formatGroupedBody(g.resolved, "Resolved · "+g.alertname+" · ", "resolved", 100)
	}

	h.setURL(&req, representative)
	req.Media = pushward.MediaImage(text.SanitizeURL(representative.ImageURL))
	req.Metadata = h.buildGroupedMetadata(g, representative)
	return req
}

// formatGroupedBody builds a body that lists instance names, fitting within maxLen.
// prefix is prepended (e.g. "Resolved · "). status is used for the fallback count message.
func formatGroupedBody(alerts []alert, prefix, status string, maxLen int) string {
	instances := collectInstanceSlice(alerts)

	if len(instances) > 0 {
		budget := maxLen - utf8.RuneCountInString(prefix)
		var included []string
		used := 0
		for i, inst := range instances {
			if budget <= 0 {
				break
			}
			sep := 0
			if len(included) > 0 {
				sep = 2 // ", "
			}
			instLen := utf8.RuneCountInString(inst)
			remaining := len(instances) - i - 1

			if remaining > 0 {
				// Reserve space for ", +N more" suffix.
				moreLen := 8 + len(strconv.Itoa(remaining)) // len(", +") + digits + len(" more")
				if used+sep+instLen+moreLen > budget {
					break
				}
			} else {
				if used+sep+instLen > budget {
					break
				}
			}
			included = append(included, inst)
			used += sep + instLen
		}

		if len(included) > 0 {
			result := prefix + strings.Join(included, ", ")
			if len(included) < len(instances) {
				result += fmt.Sprintf(", +%d more", len(instances)-len(included))
			}
			return result
		}
	}

	// Fallback: use summary if all alerts share the same one, else count.
	summary := firstNonEmptySummary(alerts)
	if summary != "" && allSummariesEqual(alerts) {
		return text.Truncate(prefix+summary, maxLen)
	}
	return fmt.Sprintf("%s%d alerts %s", prefix, len(alerts), status)
}

// collectInstanceSlice returns instance labels from alerts (non-empty, preserving order).
func collectInstanceSlice(alerts []alert) []string {
	var out []string
	for _, a := range alerts {
		if inst := a.Labels["instance"]; inst != "" {
			out = append(out, inst)
		}
	}
	return out
}

// firstNonEmptySummary returns the first non-empty summary annotation from the given alerts.
func firstNonEmptySummary(alerts []alert) string {
	for _, a := range alerts {
		if s := a.Annotations["summary"]; s != "" {
			return s
		}
	}
	return ""
}

// allSummariesEqual returns true if all non-empty summaries in alerts are identical.
func allSummariesEqual(alerts []alert) bool {
	var first string
	found := false
	for _, a := range alerts {
		s := a.Annotations["summary"]
		if s == "" {
			continue
		}
		if !found {
			first = s
			found = true
		} else if s != first {
			return false
		}
	}
	return true
}

// setURL picks the first http(s) dashboard/panel/generator URL.
func (h *Handler) setURL(req *pushward.SendNotificationRequest, a alert) {
	for _, candidate := range []string{a.DashboardURL, a.PanelURL, a.GeneratorURL} {
		if u := text.SanitizeURL(candidate); u != "" {
			req.URL = u
			return
		}
	}
}

// buildAlertMetadata builds metadata for a single-alert notification.
func (h *Handler) buildAlertMetadata(a alert) map[string]string {
	meta := newMetaMap()
	for _, key := range []string{"alertname", "severity", "instance", "job", "job_name", "namespace", "cluster", "pod", "container", "service"} {
		meta.add(key, a.Labels[key])
	}
	meta.add("fingerprint", a.Fingerprint)
	for _, k := range slices.Sorted(maps.Keys(a.Annotations)) {
		meta.add("annotation_"+k, a.Annotations[k])
	}
	meta.add("starts_at", normalizeTimestamp(a.StartsAt))
	meta.add("silence_url", text.SanitizeURL(a.SilenceURL))
	meta.add("generator_url", text.SanitizeURL(a.GeneratorURL))
	meta.add("values", formatValues(a))
	return meta.result()
}

// buildGroupedMetadata builds metadata for a grouped notification.
// Each alert gets a dedicated metadata entry with all its details packed into
// one value, matching the level of detail that Pushover/other tools provide.
func (h *Handler) buildGroupedMetadata(g *alertGroup, representative alert) map[string]string {
	meta := newMetaMap()
	allAlerts := slices.Concat(g.firing, g.resolved)

	// Fixed entries first so they are never evicted by the slot cap.
	meta.add("firing_count", strconv.Itoa(len(g.firing)))
	meta.add("resolved_count", strconv.Itoa(len(g.resolved)))
	meta.add("alertname", representative.Labels["alertname"])
	meta.add("starts_at", normalizeTimestamp(representative.StartsAt))
	meta.add("silence_url", text.SanitizeURL(representative.SilenceURL))
	meta.add("generator_url", text.SanitizeURL(representative.GeneratorURL))

	for _, key := range []string{"severity", "job", "job_name", "namespace", "cluster", "grafana_folder"} {
		if v := commonLabelValue(allAlerts, key); v != "" {
			meta.add(key, v)
		}
	}

	// Per-instance detail: fill remaining slots with packed alert data.
	for _, a := range allAlerts {
		inst := a.Labels["instance"]
		if inst == "" {
			inst = a.Fingerprint
		}
		meta.add(inst, formatAlertDetail(a))
	}

	return meta.result()
}

// formatAlertDetail packs all useful fields from a single alert into a
// human-readable string for display in the metadata section.
func formatAlertDetail(a alert) string {
	var b strings.Builder

	switch a.Status {
	case "firing":
		b.WriteString("Firing")
	case "resolved":
		b.WriteString("Resolved")
	default:
		b.WriteString(a.Status)
	}

	if s := a.Annotations["summary"]; s != "" {
		b.WriteString(" · ")
		b.WriteString(s)
	}

	if v := formatValues(a); v != "" {
		b.WriteString("\nValues: ")
		b.WriteString(v)
	}

	// Annotations in sorted order for stable output (skip summary, already shown).
	for _, k := range slices.Sorted(maps.Keys(a.Annotations)) {
		if k == "summary" {
			continue
		}
		if v := a.Annotations[k]; v != "" {
			b.WriteString("\n")
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
		}
	}

	if s := text.SanitizeURL(a.SilenceURL); s != "" {
		b.WriteString("\nSilence: ")
		b.WriteString(s)
	}

	return b.String()
}

// formatValues returns a compact representation of an alert's metric values.
// Prefers the typed Values map over the deprecated ValueString field.
func formatValues(a alert) string {
	if len(a.Values) > 0 {
		var parts []string
		for _, k := range slices.Sorted(maps.Keys(a.Values)) {
			parts = append(parts, k+" = "+formatAny(a.Values[k]))
		}
		return strings.Join(parts, ", ")
	}
	return a.ValueString
}

// formatAny renders a value for display, using decimal notation for floats
// to avoid scientific notation (e.g. 1234567 instead of 1.234567e+06).
func formatAny(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}

// normalizeTimestamp parses an RFC3339 timestamp and returns it normalised to
// UTC. Returns the original string if it cannot be parsed, so callers always
// get something displayable. Uses RFC3339Nano because Grafana emits
// fractional-second timestamps that time.RFC3339 rejects.
func normalizeTimestamp(s string) string {
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		slog.Warn("unparseable startsAt timestamp", "value", s)
		return s
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// commonLabelValue returns the label value if it is identical across all alerts, else "".
func commonLabelValue(alerts []alert, key string) string {
	if len(alerts) == 0 {
		return ""
	}
	first := alerts[0].Labels[key]
	if first == "" {
		return ""
	}
	for _, a := range alerts[1:] {
		if a.Labels[key] != first {
			return ""
		}
	}
	return first
}
