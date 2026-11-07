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

	"github.com/mac-lucky/pushward-integrations/relay/internal/ack"
	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overrides"
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
	Alerts   []alert `json:"alerts"`
	GroupKey string  `json:"groupKey"`
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
	// Unsent marks a last push whose send failed: it equals no state, so the
	// next webhook goes out, and its alerts are still those of the push before.
	Unsent bool `json:"unsent,omitempty"`
}

// With ?retract=1 an alert name keeps its state in rows by sub key. lastSub
// holds the name's last push, which the dedup check and the ack repeats
// compare against, as the "" row does without retract=1. A push without
// retract=1 drops it, and the "" row it writes is the last push again. Each
// notification group of the name has a row of its own (groupSubPrefix plus
// its hashed groupKey) with the alerts it reported firing, and a webhook
// writes only its own group's row, so concurrent groups of one name never
// overwrite each other.
const (
	lastSub        = "last"
	groupSubPrefix = "g-"
)

// groupSub is the sub key of a notification group's row. A payload without a
// groupKey counts as one group.
func groupSub(groupKey string) string { return groupSubPrefix + text.HashHex(groupKey, 8) }

func (s *alertGroupState) equals(other *alertGroupState) bool {
	if other == nil || other.Unsent {
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

// PushWard's metadata limits, all four of which the server enforces on
// POST /notifications and any one of which rejects the whole request with a 400
// - the notification is then simply lost, since the relay has nowhere to retry
// a rejected payload to.
//
// maxMetaBytes is the one that actually binds. The per-value cap is 4096, but
// twenty values at even 512 come to 10 KB, which is over the aggregate budget:
// filling the slots with long Grafana annotations used to build a payload the
// server refused. So the budget is tracked as it is spent rather than
// approximated by a per-value constant, and maxMetaValueLen is now only a
// readability cap on any single entry.
const (
	maxMetaEntries  = 20
	maxMetaKeyLen   = 64
	maxMetaValueLen = 512
	maxMetaBytes    = 8 * 1024
)

// metaMap is a capped metadata map that silently drops entries once either the
// slot count or the byte budget is exhausted. Dropping the tail is deliberate:
// callers add the fixed, most useful entries first (see buildGroupedMetadata),
// so what gets dropped is the least important detail rather than the alert.
type metaMap struct {
	m     map[string]string
	bytes int
}

func newMetaMap() *metaMap { return &metaMap{m: make(map[string]string, maxMetaEntries)} }

func (m *metaMap) add(k, v string) {
	if v == "" || m.full() {
		return
	}
	k = metaKey(k)
	// Bail before TruncateHard, which walks the whole string and allocates a
	// []rune over it: a kept value is at least one byte, so if the key alone
	// does not fit there is nothing to truncate for. Grafana annotations are
	// routinely kilobytes, and a big alert group drops far more than it keeps.
	if m.bytes+len(k)+1 > maxMetaBytes {
		return
	}
	v = text.TruncateHard(v, maxMetaValueLen)
	// Same accounting the server does: key plus value, in bytes not runes.
	cost := len(k) + len(v)
	if m.bytes+cost > maxMetaBytes {
		return
	}
	m.m[k] = v
	m.bytes += cost
}

// full reports whether neither a slot nor a single byte remains. Entries are
// only ever added, never evicted, so a caller looping over alerts can stop
// building values once this is true.
func (m *metaMap) full() bool {
	return len(m.m) >= maxMetaEntries || m.bytes+1 > maxMetaBytes
}

// metaKey is the stored form of a key. buildGroupedMetadata synthesises keys
// from Grafana labels (`instance (firing)`, `instance fingerprint`), which run
// past the server's 64-rune key cap.
func metaKey(k string) string { return text.TruncateHard(k, maxMetaKeyLen) }

// has reports whether a key is already taken, so callers can disambiguate
// before spending a slot on an entry that would overwrite another. It applies
// the same truncation `add` does - otherwise two keys that differ only past
// rune 64 would look distinct here and collide in the map.
func (m *metaMap) has(k string) bool {
	_, ok := m.m[metaKey(k)]
	return ok
}

func (m *metaMap) result() map[string]string {
	if len(m.m) > 0 {
		return m.m
	}
	return nil
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
	return "Grafana" + text.SepDot + strings.Join(parts, ", ")
}

func (h *Handler) handleWebhook(ctx context.Context, input *struct {
	Body grafanaPayload
},
) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, "grafana")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	cl := h.clients.Get(userKey)

	// Grafana is notification-only, so channels=activity leaves nothing to send.
	ov := overrides.FromContext(ctx)
	if !ov.AllowsNotification() {
		return humautil.NewOK(), nil
	}

	// Group alerts by alertname, preserving insertion order.
	groups, groupOrder := h.groupAlerts(log, input.Body.Alerts)

	var apiErr error
	retract := ov.Retract()
	for _, alertname := range groupOrder {
		g := groups[alertname]

		// Check state for dedup. Use the collision-resistant hash (matching the
		// CollapseID derivation) - text.Slug collapses distinct alertnames like
		// "High CPU"/"high_cpu" to one key, cross-contaminating dedup.
		stateKey := text.SlugHash("grafana", alertname, 6)

		// A group with nothing firing left stops the repeats of its alert
		// (?ack=1) ahead of the dedup check. The tag comes from the collapse
		// id, so this needs no state and works after a restart.
		firing := len(g.firing) > 0
		if !firing {
			ack.Cancel(ctx, cl, log, stateKey)
		}

		currentState := h.buildState(g)
		ownSub := groupSub(input.Body.GroupKey)
		var prev *alertGroupState
		var tracked, elsewhere bool
		if ov.Retract() {
			prev, tracked, elsewhere = h.loadName(ctx, userKey, stateKey, ownSub, g, log)
			if firing {
				// Written before the push goes out, so another group of the
				// name resolving meanwhile sees it and does not retract.
				h.updateState(ctx, userKey, stateKey, ownSub, currentState, log)
			}
		} else {
			prev = h.loadState(ctx, userKey, stateKey, log)
		}
		if currentState.equals(prev) {
			if ov.Retract() && !firing {
				// The name's last push already resolved these alerts, so
				// this group's row goes as it would after its own resolve.
				h.updateState(ctx, userKey, stateKey, ownSub, currentState, log)
			}
			log.Debug("alert state unchanged, skipping", "alertname", stateKey)
			continue
		}

		// ?retract=1 takes the alert name's push back instead of sending
		// Resolved over it, when this group was recorded firing and no other
		// group of the name may still fire. Any doubt, and a retract that
		// reached no device, sends the Resolved. The collapse id is stateKey.
		if !firing && retract && tracked && !elsewhere {
			if h.clients.Retract(ctx, userKey, log, stateKey) {
				h.saveName(ctx, userKey, stateKey, ownSub, currentState, log)
				continue
			}
			// The rest of this webhook resolves without retracting: what kept
			// this one from reaching (the account's rate limit, no device)
			// holds for them too, and each would wait out its own timeout.
			retract = false
		}

		// Build and send notification.
		var req pushward.SendNotificationRequest
		if len(g.firing)+len(g.resolved) == 1 {
			req = h.buildSingleNotification(g)
		} else {
			req = h.buildGroupedNotification(g)
		}
		req.Level = ov.LevelOr(req.Level)

		// Only a fingerprint that was not firing before starts repeats. A
		// group that changed by resolving some of its alerts goes out plain,
		// so an alert already acknowledged does not start over.
		identity := ""
		if newlyFiring(g, prev) {
			identity = stateKey
		}
		if err := ack.Send(ctx, cl, log, req, identity); err != nil {
			log.Error("failed to send notification", "alertname", alertname, "error", err)
			apiErr = err
			if ov.Retract() {
				// The push may have reached the devices or not, so the last
				// push is marked unsent: it compares as changed, keeps the
				// alerts acknowledged repeats compare against, and unlike no
				// row at all keeps a "" row from standing in for it.
				failed := alertGroupState{Unsent: true}
				if prev != nil {
					failed = *prev
					failed.Unsent = true
				}
				// Detached, as a send cut short by the caller leaving ends
				// with its context already done.
				wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				h.setState(wctx, userKey, stateKey, lastSub, &failed, log)
				cancel()
			}
			continue
		}
		log.Info("notification sent", "alertname", alertname,
			"firing", len(g.firing), "resolved", len(g.resolved))

		// Store state after successful send.
		if ov.Retract() {
			h.saveName(ctx, userKey, stateKey, ownSub, currentState, log)
		} else {
			h.updateState(ctx, userKey, stateKey, "", currentState, log)
			// The "" row is the last push now. Only this row: the g- rows
			// still show what other groups have firing.
			if err := h.store.Delete(ctx, "grafana", userKey, stateKey, lastSub); err != nil {
				log.Warn("failed to delete alert state", "key", stateKey, "error", err)
			}
		}
	}

	// The loop above keeps going after a failure, so apiErr holds the last one.
	// Any of them being an upstream refusal of this caller's key means the rest
	// were too, so mapping the last is as good as mapping the first.
	if apiErr != nil {
		return nil, humautil.UpstreamError(apiErr)
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

// loadState returns the previously stored state of an alert group, or nil
// when there is none or it cannot be read. nil compares as changed, so a store
// error never drops a notification.
func (h *Handler) loadState(ctx context.Context, userKey, stateKey string, log *slog.Logger) *alertGroupState {
	raw, err := h.store.Get(ctx, "grafana", userKey, stateKey, "")
	if err != nil {
		log.Warn("failed to read alert state", "key", stateKey, "error", err)
		return nil
	}
	if raw == nil {
		return nil // no prior state
	}
	return decodeState(raw, stateKey, log)
}

func decodeState(raw json.RawMessage, stateKey string, log *slog.Logger) *alertGroupState {
	var s alertGroupState
	if err := json.Unmarshal(raw, &s); err != nil {
		log.Warn("failed to decode alert state", "key", stateKey, "error", err)
		return nil
	}
	return &s
}

// loadName reads every row of an alert name (?retract=1) in one query. last is
// the name's last push. tracked reports that this group's row has alerts
// firing. elsewhere reports that another alert of the name may still fire:
// another group's row lists one this webhook does not resolve, a row could not
// be read, or there is a "" row. That one was written without retract=1 or
// before groups had rows of their own, and shows only the group that posted
// last, so other groups may fire unrecorded until it expires. Until the
// name's first push or retract writes the "last" row it is also the last push,
// so a name already pushed is not pushed again, nor its acknowledged repeats
// started over.
func (h *Handler) loadName(ctx context.Context, userKey, stateKey, ownSub string, g *alertGroup, log *slog.Logger) (last *alertGroupState, tracked, elsewhere bool) {
	rows, err := h.store.GetGroup(ctx, "grafana", userKey, stateKey)
	if err != nil {
		log.Warn("failed to read alert state", "key", stateKey, "error", err)
		return nil, false, true
	}
	older, elsewhere := rows[""]
	if raw, ok := rows[lastSub]; ok {
		last = decodeState(raw, stateKey, log)
	} else if elsewhere {
		last = decodeState(older, stateKey, log)
	}
	for sub, raw := range rows {
		if !strings.HasPrefix(sub, groupSubPrefix) {
			continue
		}
		switch s := decodeState(raw, stateKey, log); {
		case s == nil:
			elsewhere = true
		case sub == ownSub:
			tracked = len(s.Firing) > 0
		case len(firingElsewhere(g, s)) > 0:
			elsewhere = true
		}
	}
	return last, tracked, elsewhere
}

// saveName stores the alert name's last push once it went out or was
// retracted, and drops this group's row once nothing in it fires. A resolve
// is kept as the last push too: without it a "" row would count as the last
// push again, and the alert firing anew would be skipped as a repeat of it.
func (h *Handler) saveName(ctx context.Context, userKey, stateKey, ownSub string, current *alertGroupState, log *slog.Logger) {
	h.setState(ctx, userKey, stateKey, lastSub, current, log)
	if len(current.Firing) == 0 {
		h.updateState(ctx, userKey, stateKey, ownSub, current, log)
	}
}

// newlyFiring reports whether g fires an alert that prev did not have firing.
// A nil prev (no state, or an unreadable one) makes every firing alert new.
func newlyFiring(g *alertGroup, prev *alertGroupState) bool {
	for _, a := range g.firing {
		if prev == nil || !slices.Contains(prev.Firing, a.Fingerprint) {
			return true
		}
	}
	return false
}

// firingElsewhere lists the alerts prev had firing that g neither fires nor
// resolves: those of another group under the same alert name. An alert g
// resolves is resolved for every group that holds it.
func firingElsewhere(g *alertGroup, prev *alertGroupState) []string {
	if prev == nil {
		return nil
	}
	var out []string
	for _, fp := range prev.Firing {
		in := func(a alert) bool { return a.Fingerprint == fp }
		if !slices.ContainsFunc(g.firing, in) && !slices.ContainsFunc(g.resolved, in) {
			out = append(out, fp)
		}
	}
	return out
}

// updateState stores current in the row sub of the alert name, or deletes the
// row when nothing in current fires.
func (h *Handler) updateState(ctx context.Context, userKey, stateKey, sub string, current *alertGroupState, log *slog.Logger) {
	if len(current.Firing) == 0 {
		// All resolved - delete state.
		if err := h.store.Delete(ctx, "grafana", userKey, stateKey, sub); err != nil {
			log.Warn("failed to delete alert state", "key", stateKey, "error", err)
		}
		return
	}
	h.setState(ctx, userKey, stateKey, sub, current, log)
}

// setState stores current in the row sub of the alert name for StaleTimeout.
func (h *Handler) setState(ctx context.Context, userKey, stateKey, sub string, current *alertGroupState, log *slog.Logger) {
	data, err := json.Marshal(current)
	if err != nil {
		log.Warn("failed to marshal alert state", "key", stateKey, "error", err)
		return
	}
	if err := h.store.Set(ctx, "grafana", userKey, stateKey, sub, data, h.config.StaleTimeout); err != nil {
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

	summary := a.Annotations["summary"]

	subtitle := "Grafana"
	if instance := a.Labels["instance"]; instance != "" {
		subtitle = "Grafana" + text.SepDot + instance
	}

	req := pushward.SendNotificationRequest{
		Title:      g.alertname,
		Subtitle:   text.Truncate(subtitle, 80),
		ThreadID:   "grafana",
		CollapseID: text.SlugHash("grafana", g.alertname, 6),
		Source:     "grafana",
		Push:       pushward.BoolPtr(true),
	}

	switch a.Status {
	case "firing":
		req.Body = text.Truncate(summary, 120)
		req.Level = pushward.LevelActive
	case "resolved":
		req.Body = "Resolved" + text.SepDot + g.alertname
		if summary != "" {
			req.Body = "Resolved" + text.SepDot + g.alertname + text.SepDot + text.Truncate(summary, 80)
		}
		req.Level = pushward.LevelPassive
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
		Push:       pushward.BoolPtr(true),
	}

	if len(g.firing) > 0 {
		req.Level = pushward.LevelActive
		req.Body = formatGroupedBody(g.firing, g.alertname+text.SepDot, "firing", 120)
	} else {
		req.Level = pushward.LevelPassive
		req.Body = formatGroupedBody(g.resolved, "Resolved"+text.SepDot+g.alertname+text.SepDot, "resolved", 100)
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
	// Disambiguate when the same instance appears in both firing and resolved
	// (or twice with the same status) so one entry doesn't overwrite the other.
	for _, a := range allAlerts {
		// formatAlertDetail builds a whole string per alert; stop once nothing
		// more can be stored rather than formatting a 500-alert group to throw
		// ~485 of them away.
		if meta.full() {
			break
		}
		inst := a.Labels["instance"]
		if inst == "" {
			inst = a.Fingerprint
		}
		key := inst
		if meta.has(key) {
			key = inst + " (" + a.Status + ")"
			if meta.has(key) {
				key = inst + " " + a.Fingerprint
			}
		}
		meta.add(key, formatAlertDetail(a))
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
		b.WriteString(text.SepDot)
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
