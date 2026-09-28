package universalhook

import (
	"context"
	"encoding/json"
	"strings"
	"unicode"

	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overrides"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// Mapping statuses as universal_applies_total labels them.
const (
	statusNew       = "new"
	statusPending   = "pending"
	statusConfirmed = "confirmed"
	statusRejected  = "rejected"
)

// Text caps. Flatten has already cut every value to universal.MaxValueRunes.
const (
	maxTitleRunes = 100
	maxStateRunes = 100
)

// activity names the Live Activity an event lands on, and its state row.
type activity struct {
	slug, mapKey string
}

// deliver sends one payload through a mapping. A failed delivery is the
// request's failure, so the sender retries it. The error stays the client's:
// refusal reads its status, and handleWebhook turns it into the response.
func (h *Handler) deliver(ctx context.Context, r *request, m universal.Mapping, status string) (*humautil.WebhookResponse, error) {
	ev := universal.Apply(m, r.fields, r.source())
	metrics.UniversalAppliesTotal.WithLabelValues(status, string(ev.Kind)).Inc()
	countFallbacks(m, ev)

	resp := humautil.NewOK()
	var err error
	switch ev.Kind {
	case universal.KindAlert, universal.KindProgress:
		resp, err = h.deliverActivity(ctx, r, m, ev)
	default:
		err = h.deliverNotification(ctx, r, m, ev)
	}
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// countFallbacks records severity and lifecycle values the mapping's table did
// not cover. A role the mapping leaves out falls back by design and is not
// counted.
func countFallbacks(m universal.Mapping, ev universal.Event) {
	if m.Paths[universal.RoleSeverity] != "" && ev.SevFrom != universal.FromTable {
		metrics.UniversalValueFallbackTotal.WithLabelValues(string(universal.RoleSeverity), ev.SevFrom).Inc()
	}
	if m.Paths[universal.RoleLifecycle] != "" && ev.LcFrom != universal.FromTable {
		metrics.UniversalValueFallbackTotal.WithLabelValues(string(universal.RoleLifecycle), ev.LcFrom).Inc()
	}
}

func (h *Handler) deliverNotification(ctx context.Context, r *request, m universal.Mapping, ev universal.Event) error {
	if !overrides.FromContext(ctx).AllowsNotification() {
		return nil
	}
	return h.notify(ctx, r, m, ev, "", levelOf(m, ev), "")
}

// notify sends ev as a notification. slug links it to a card that exists;
// prefix, when set, leads the body ("Resolved").
func (h *Handler) notify(ctx context.Context, r *request, m universal.Mapping, ev universal.Event, slug, level, prefix string) error {
	req := pushward.SendNotificationRequest{
		Title:        text.TruncateHard(ev.Title, maxTitleRunes),
		Body:         ev.Body,
		URL:          ev.URL,
		Source:       r.source(),
		ThreadID:     threadID(r.source()),
		Level:        overrides.FromContext(ctx).LevelOr(level),
		ActivitySlug: slug,
		Push:         pushward.BoolPtr(true),
	}
	if prefix != "" {
		req.Body = prefix + text.SepDot + ev.Body
	}
	if ev.CorrelationKey != "" {
		req.CollapseID = activitySlug(r.source(), ev.CorrelationKey)
	}
	return h.clients.SendNotification(ctx, r.key, r.log, req)
}

func threadID(source string) string {
	if source == "" {
		return "universal"
	}
	return "universal-" + source
}

// levelOf maps severity onto an interruption level. Without a severity field
// every event is active, the server default.
func levelOf(m universal.Mapping, ev universal.Event) string {
	if m.Paths[universal.RoleSeverity] == "" || ev.SevFrom == universal.FromDefault {
		return pushward.LevelActive
	}
	switch ev.Severity {
	case universal.SeverityCritical:
		return pushward.LevelTimeSensitive
	case universal.SeverityInfo:
		return pushward.LevelPassive
	}
	return pushward.LevelActive
}

// activitySlug keys a card on the source and the correlation key, or the
// title when there is none. The fingerprint is left out on purpose: a firing
// and a resolved payload often differ in shape and must still land on one card.
func activitySlug(source, id string) string {
	prefix := "u"
	if source != "" {
		prefix = "u-" + source
	}
	return text.SlugHash(prefix, id, 6)
}

// hasValue reports whether the payload has a value Apply would read for role,
// which tells a real title from the fallback Apply puts in its place.
func hasValue(m universal.Mapping, fields []universal.Field, role universal.Role) bool {
	path := m.Paths[role]
	if path == "" {
		return false
	}
	for _, f := range fields {
		if f.Path == path {
			return strings.TrimSpace(f.Value) != "" && universal.ClassOf(f.Path, f) != universal.ClassSecret
		}
	}
	return false
}

func (h *Handler) deliverActivity(ctx context.Context, r *request, m universal.Mapping, ev universal.Event) (*humautil.WebhookResponse, error) {
	id := ev.CorrelationKey
	if id == "" && hasValue(m, r.fields, universal.RoleTitle) {
		id = ev.Title
	}
	if id == "" {
		// Nothing to find the card again by, so a resolved event could never
		// end it.
		if err := h.deliverNotification(ctx, r, m, ev); err != nil {
			return nil, err
		}
		return ignored(humautil.StatusIgnoredActivity, "the mapping has no correlation or title value to key a Live Activity on"), nil
	}
	slug := activitySlug(r.source(), id)
	a := activity{slug: slug, mapKey: "act:" + slug}
	if ev.Lifecycle == universal.LifecycleEnded {
		return humautil.NewOK(), h.end(ctx, r, m, ev, a)
	}
	return humautil.NewOK(), h.ongoing(ctx, r, m, ev, a)
}

func (h *Handler) ongoing(ctx context.Context, r *request, m universal.Mapping, ev universal.Event, a activity) error {
	ov := overrides.FromContext(ctx)
	alert := ev.Kind == universal.KindAlert
	if !ov.AllowsActivity() {
		// The push is the only delivery left, so its failure is the
		// request's.
		if ov.NotifyFallback(alert) {
			return h.notify(ctx, r, m, ev, "", levelOf(m, ev), "")
		}
		return nil
	}

	// A new event cancels an end still pending from a resolved one.
	h.ender.StopTimer(r.key, a.mapKey)

	// On a store error the card is treated as new: a duplicate create is an
	// upsert, while a dropped alert is lost.
	card, tracked, err := h.card(ctx, r, a)
	if err != nil {
		r.log.Warn("state store read failed, treating the activity as new", "slug", a.slug, "error", err)
	}
	isNew := !tracked
	cl := h.clients.Get(r.key)
	if isNew {
		err := cl.CreateActivity(ctx, a.slug, text.TruncateHard(ev.Title, maxTitleRunes),
			ov.PriorityOr(h.config.Priority), int(h.config.CleanupDelay.Seconds()), int(h.config.StaleTimeout.Seconds()),
			h.config.CreateOptions()...)
		if err != nil {
			r.log.Error("failed to create activity", "slug", a.slug, "error", err)
			// A card the server refuses for good (the activity limit, a key
			// without activity rights) would be refused on every retry; the
			// event still reaches the user as a plain notification.
			if _, permanent := refusal(err); permanent {
				if nerr := h.notify(ctx, r, m, ev, "", levelOf(m, ev), ""); nerr == nil {
					return nil
				}
			}
			return err
		}
	}

	// A progress event without a value keeps the bar where the last one left
	// it instead of resetting it to zero.
	if ev.Progress == nil && card.Progress != nil {
		ev.Progress = card.Progress
	}
	content := h.progressContent(r, ev)
	if alert {
		content = h.alertContent(r, ev, isNew)
	}
	if err := cl.UpdateActivity(ctx, a.slug, pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}); err != nil {
		r.log.Error("failed to update activity", "slug", a.slug, "error", err)
		return err
	}
	h.remember(ctx, r, a, cardState{Slug: a.slug, Progress: ev.Progress})

	// A new alert interrupts; a progress start does not.
	if isNew && ov.NotifyFallback(alert) {
		// The card already carries the event, so a failed push is logged
		// by the pool and not the request's failure.
		_ = h.notify(ctx, r, m, ev, a.slug, levelOf(m, ev), "")
	}
	return nil
}

// remember records the open card, so a later event updates it and a resolved
// one can end it. It is written below the AllowsActivity gate only, which is
// what makes Ender.EndIfTracked sound here.
func (h *Handler) remember(ctx context.Context, r *request, a activity, c cardState) {
	data, _ := json.Marshal(c)
	if err := h.store.Set(ctx, provider, r.key, a.mapKey, "", data, h.config.StaleTimeout); err != nil {
		r.log.Warn("state store write failed", "slug", a.slug, "error", err)
	}
}

// cardState is what the relay keeps about an open card.
type cardState struct {
	Slug     string
	Progress *float64 `json:",omitempty"`
}

// card reads the open card's state; tracked is false when there is none or
// the read failed. A row too old to hold Progress still counts as tracked.
func (h *Handler) card(ctx context.Context, r *request, a activity) (cardState, bool, error) {
	var c cardState
	raw, err := h.store.Get(ctx, provider, r.key, a.mapKey, "")
	if err != nil || raw == nil {
		return c, false, err
	}
	_ = json.Unmarshal(raw, &c)
	return c, true, nil
}

func (h *Handler) end(ctx context.Context, r *request, m universal.Mapping, ev universal.Event, a activity) error {
	ov := overrides.FromContext(ctx)
	content, outcome := h.finalContent(r, ev)

	var tracked bool
	if ov.AllowsActivity() {
		ok, err := h.store.Exists(ctx, provider, r.key, a.mapKey, "")
		if err != nil {
			r.log.Warn("state store read failed, not ending an activity", "slug", a.slug, "error", err)
		}
		if ok {
			h.ender.ScheduleEnd(r.key, a.mapKey, a.slug, content)
			tracked = true
		}
	} else {
		// An earlier request may have opened the card; channels=notification
		// on this one must not leave it hanging until its stale timeout.
		tracked = h.ender.EndIfTracked(ctx, r.log, r.key, a.mapKey, a.slug, content)
	}

	if !ov.NotifyFallback(true) {
		return nil
	}
	level := pushward.LevelPassive
	if ev.Kind == universal.KindProgress && failed(ev.LifecycleRaw) {
		level = pushward.LevelActive
	}
	slug := ""
	if tracked {
		slug = a.slug
	}
	err := h.notify(ctx, r, m, ev, slug, level, outcome)
	if tracked {
		// The card carries the outcome.
		return nil
	}
	return err
}

func (h *Handler) alertContent(r *request, ev universal.Event, isNew bool) pushward.Content {
	c := pushward.Content{
		Template:    pushward.TemplateAlert,
		Progress:    1,
		State:       text.TruncateHard(ev.Body, maxStateRunes),
		Icon:        pushward.SeverityIcon(ev.Severity, "exclamationmark.triangle.fill"),
		Subtitle:    r.source(),
		AccentColor: pushward.SeverityColor(ev.Severity),
		Severity:    ev.Severity,
		URL:         ev.URL,
	}
	// Only the first frame: senders repeat a firing alert, and the card
	// should keep counting from when it started.
	if isNew {
		c.FiredAt = pushward.Int64Ptr(h.now().Unix())
	}
	return c
}

func (h *Handler) progressContent(r *request, ev universal.Event) pushward.Content {
	state := text.Capitalize(ev.LifecycleRaw)
	if state == "" {
		state = ev.Body
	}
	c := pushward.Content{
		Template:    pushward.TemplateGeneric,
		State:       text.TruncateHard(state, maxStateRunes),
		Icon:        "arrow.triangle.2.circlepath",
		Subtitle:    r.source(),
		AccentColor: pushward.ColorBlue,
		URL:         ev.URL,
	}
	if ev.Progress != nil {
		c.Progress = *ev.Progress
	}
	return c
}

// finalContent is the frame a card ends on, and the word the end notification
// leads with.
func (h *Handler) finalContent(r *request, ev universal.Event) (pushward.Content, string) {
	if ev.Kind == universal.KindAlert {
		return pushward.Content{
			Template:    pushward.TemplateAlert,
			Progress:    1,
			State:       "Resolved",
			Icon:        "checkmark.circle.fill",
			Subtitle:    r.source(),
			AccentColor: pushward.ColorGreen,
			Severity:    universal.SeverityInfo,
			URL:         ev.URL,
		}, "Resolved"
	}
	c := pushward.Content{
		Template:    pushward.TemplateGeneric,
		Progress:    1,
		State:       "Done",
		Icon:        "checkmark.circle.fill",
		Subtitle:    r.source(),
		AccentColor: pushward.ColorGreen,
		URL:         ev.URL,
	}
	if failed(ev.LifecycleRaw) {
		c.State = text.TruncateHard(text.Capitalize(ev.LifecycleRaw), maxStateRunes)
		c.Icon = "xmark.circle.fill"
		c.AccentColor = pushward.ColorRed
		if ev.Progress != nil {
			c.Progress = *ev.Progress
		}
	}
	return c, c.State
}

var failureWords = map[string]bool{
	"fail": true, "failed": true, "failure": true, "error": true, "errored": true,
	"aborted": true, "abort": true, "timeout": true, "timedout": true,
}

// failed reports whether a lifecycle value that ended a run says it went
// wrong: "failed", "build.error", "TIMED_OUT", "cancelled".
func failed(raw string) bool {
	n := universal.NormValue(raw)
	if strings.Contains(n, "timed_out") {
		return true
	}
	for _, w := range strings.FieldsFunc(n, func(c rune) bool { return !unicode.IsLetter(c) && !unicode.IsDigit(c) }) {
		if failureWords[w] || strings.HasPrefix(w, "cancel") {
			return true
		}
	}
	return false
}
