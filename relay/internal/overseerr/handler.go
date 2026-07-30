package overseerr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/mediathread"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overrides"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.OverseerrConfig
	ender   *lifecycle.Ender
}

// RegisterRoutes registers the Overseerr webhook endpoint and returns the Handler.
func RegisterRoutes(api huma.API, store state.Store, clients *client.Pool, cfg *config.OverseerrConfig) *Handler {
	h := &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "overseerr", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
	}
	humautil.RegisterWebhook(api, "/overseerr", "post-overseerr-webhook",
		"Receive Overseerr media request webhook",
		"Processes Overseerr, Jellyseerr and Seerr media request lifecycle events.",
		[]string{"Overseerr"}, h.handleWebhook)
	return h
}

func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

// skipReason says why a webhook was not acted on in full. The values reach the
// caller as the response body's detail field and are used as a metric label, so
// they are named constants rather than log prose: editing one of these is a
// change to the public contract, not to a log line.
type skipReason string

const (
	reasonNoMediaType  skipReason = "media.media_type is empty" + mediaBlockHint
	reasonBadMediaType skipReason = "media.media_type is not movie or tv"
	reasonNoTmdbID     skipReason = "media.tmdbId is empty" + mediaBlockHint
	reasonBadTmdbID    skipReason = "media.tmdbId is not a number"
	reasonUnknownType  skipReason = "unsupported notification_type"
	reasonNoChannel    skipReason = "the notification channel is suppressed for this request"
)

// mediaBlockHint names the misconfiguration behind almost every occurrence: a
// webhook JSON payload whose "{{media}}" key holds an empty object is sent
// through verbatim as `"media": {}`, indistinguishable from a media-less event
// unless the response says which field was wanted.
const mediaBlockHint = "; the webhook JSON payload's {{media}} block has to list media_type and tmdbId"

// eventSpec is the per-notification-type shape of a request lifecycle card.
type eventSpec struct {
	step        int
	state       string
	icon        string
	accentColor string
	terminal    bool
}

func (h *Handler) handleWebhook(ctx context.Context, input *struct {
	Body overseerrPayload
},
) (*humautil.WebhookResponse, error) {
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	ctx = metrics.WithProvider(ctx, "overseerr")
	payload := &input.Body

	switch payload.NotificationType {
	case "MEDIA_PENDING":
		return h.handleEvent(ctx, userKey, log, payload, eventSpec{step: 1, state: "Requested", icon: "hourglass", accentColor: pushward.ColorOrange})
	case "MEDIA_APPROVED", "MEDIA_AUTO_APPROVED":
		return h.handleEvent(ctx, userKey, log, payload, eventSpec{step: 2, state: "Approved", icon: "checkmark.circle", accentColor: pushward.ColorBlue})
	case "MEDIA_AVAILABLE":
		return h.handleEvent(ctx, userKey, log, payload, eventSpec{step: 4, state: "Available", icon: "checkmark.circle.fill", accentColor: pushward.ColorGreen, terminal: true})
	// Step 0 is load-bearing: it is what suppresses content.Progress below, and
	// it still ships as current_step so the card shows an unstarted bar.
	case "MEDIA_DECLINED":
		return h.handleEvent(ctx, userKey, log, payload, eventSpec{step: 0, state: "Declined", icon: "xmark.circle.fill", accentColor: pushward.ColorRed, terminal: true})
	case "MEDIA_FAILED":
		return h.handleEvent(ctx, userKey, log, payload, eventSpec{step: 0, state: "Failed", icon: "xmark.circle.fill", accentColor: pushward.ColorRed, terminal: true})

	// Seerr-era types with no request lifecycle to track. They carry the media
	// object so they still thread onto that title's other notifications.
	case "MEDIA_AUTO_REQUESTED":
		return h.handleNotifyOnly(ctx, userKey, log, payload, "Auto-requested", payload.Message)
	case "ISSUE_CREATED":
		return h.handleNotifyOnly(ctx, userKey, log, payload, "Issue reported", payload.Message)
	case "ISSUE_COMMENT":
		return h.handleNotifyOnly(ctx, userKey, log, payload, "New comment", payload.Comment.Message)
	case "ISSUE_RESOLVED":
		return h.handleNotifyOnly(ctx, userKey, log, payload, "Issue resolved", "")
	case "ISSUE_REOPENED":
		return h.handleNotifyOnly(ctx, userKey, log, payload, "Issue reopened", "")

	case "TEST_NOTIFICATION":
		if err := selftest.SendTest(ctx, h.clients.Get(userKey), log, "overseerr"); err != nil {
			return nil, humautil.UpstreamError(err)
		}
		return humautil.NewOK(), nil

	default:
		log.Debug("unknown overseerr notification type", "type", payload.NotificationType)
		return ignored(humautil.StatusIgnored, reasonUnknownType), nil
	}
}

// handleEvent tracks one step of the request lifecycle. The push notification
// goes out whether or not the payload carries a usable media identity; only the
// Live Activity needs one, because the slug is keyed on it.
func (h *Handler) handleEvent(ctx context.Context, userKey string, log *slog.Logger, p *overseerrPayload, spec eventSpec) (*humautil.WebhookResponse, error) {
	ov := overrides.FromContext(ctx)
	mediaType, tmdbID, reason := p.activityKey()

	notifErr := h.notify(ctx, ov, userKey, log, p, spec.state, "")

	// A credential the notification just bounced off will bounce the activity
	// calls too, so answer now rather than spending two more round-trips on it.
	if isAuthFailure(notifErr) {
		return nil, humautil.UpstreamError(notifErr)
	}

	if !ov.AllowsActivity() {
		// A terminal event still has to close an activity that an earlier,
		// non-suppressed event opened for this media, or it hangs on the lock
		// screen until the stale TTL. Non-terminal events must not: the activity
		// is meant to stay open for the rest of the request's lifecycle.
		if spec.terminal && reason == "" {
			h.ender.EndIfTracked(ctx, log, userKey, h.mapKey(mediaType, tmdbID), h.slug(mediaType, tmdbID), h.content(p, spec))
		}
		log.Info("overseerr event", "type", spec.state)
		// The caller asked for notification-only, so the push was the sole
		// delivery path and a failed one is the request's failure. The missing
		// activity is what they wanted, so it is not reported as skipped.
		return pushOnly(notifErr)
	}

	if reason != "" {
		log.Warn("overseerr: no media identity, live activity skipped", "type", spec.state, "reason", reason,
			"media_type", text.TruncateHard(p.Media.MediaType, 64), "tmdbId", text.TruncateHard(p.Media.TmdbID, 64))
		if notifErr != nil {
			return nil, humautil.UpstreamError(notifErr)
		}
		return ignored(humautil.StatusIgnoredActivity, reason), nil
	}

	slug := h.slug(mediaType, tmdbID)
	mapKey := h.mapKey(mediaType, tmdbID)
	content := h.content(p, spec)

	// Cancel any pending two-phase end from a prior terminal event so a new
	// event for the same media (e.g. a re-request) isn't ended out from under us.
	h.ender.StopTimer(userKey, mapKey)

	name := text.TruncateHard(p.Subject, 100)
	if name == "" {
		name = "Media Request"
	}

	cl := h.clients.Get(userKey)
	if err := cl.CreateActivity(ctx, slug, name, ov.PriorityOr(h.config.Priority),
		int(h.config.CleanupDelay.Seconds()), int(h.config.StaleTimeout.Seconds())); err != nil {
		log.Error("failed to create overseerr activity", "slug", slug, "error", err)
		return nil, humautil.UpstreamError(err)
	}

	if err := cl.UpdateActivity(ctx, slug, pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	}); err != nil {
		log.Error("failed to update overseerr activity", "slug", slug, "error", err)
		return nil, humautil.UpstreamError(err)
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "overseerr", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		log.Warn("state store write failed", "error", err, "provider", "overseerr", "slug", slug)
	}

	if spec.terminal {
		h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	}

	log.Info("overseerr event", "slug", slug, "type", spec.state)
	// notifErr is deliberately dropped here: the activity carries the outcome, so
	// a failed push is logged rather than failing a webhook that did deliver.
	return humautil.NewOK(), nil
}

// handleNotifyOnly covers the event types that have no request lifecycle to
// track. The push is the only delivery path, so a failed one fails the request
// and a suppressed one is reported rather than answered "ok".
func (h *Handler) handleNotifyOnly(ctx context.Context, userKey string, log *slog.Logger, p *overseerrPayload, label, detail string) (*humautil.WebhookResponse, error) {
	ov := overrides.FromContext(ctx)
	if !ov.AllowsNotification() {
		log.Info("overseerr event", "type", label, "reason", reasonNoChannel)
		return ignored(humautil.StatusIgnored, reasonNoChannel), nil
	}
	if err := h.notify(ctx, ov, userKey, log, p, label, detail); err != nil {
		return nil, humautil.UpstreamError(err)
	}
	log.Info("overseerr event", "type", label)
	return humautil.NewOK(), nil
}

// notify sends the push for one event. Threading and metadata read the payload's
// own media fields rather than the activity key, so a payload that can group but
// not open a card still groups.
func (h *Handler) notify(ctx context.Context, ov *overrides.Overrides, userKey string, log *slog.Logger, p *overseerrPayload, label, detail string) error {
	if !ov.AllowsNotification() {
		return nil
	}

	body := label
	if detail != "" {
		body += text.SepDot + text.TruncateHard(detail, 140)
	}

	mediaType := p.threadType()
	tmdbID := ""
	if isNumeric(p.Media.TmdbID) {
		tmdbID = p.Media.TmdbID
	}

	req := pushward.SendNotificationRequest{
		Title:      "Overseerr",
		Subtitle:   text.TruncateHard(p.Subject, 100),
		Body:       body,
		ThreadID:   mediathread.ThreadID(mediaType, tmdbID, p.tvdbID()),
		CollapseID: collapseID(p.Subject, label, mediaType, tmdbID),
		Level:      ov.LevelOr(pushward.LevelActive),
		Source:     "overseerr",
		Media:      pushward.MediaImage(p.Image),
		Push:       pushward.BoolPtr(true),
		Metadata:   notificationMetadata(p, mediaType, tmdbID),
	}

	return h.clients.SendNotification(ctx, userKey, log, req)
}

// notificationMetadata bounds every value: all of them are attacker-controlled,
// and pushward-server rejects the whole notification when one is oversized.
func notificationMetadata(p *overseerrPayload, mediaType, tmdbID string) map[string]string {
	md := map[string]string{}
	if mediaType != "" {
		md["media_type"] = mediaType
	}
	if tmdbID != "" {
		md["tmdb_id"] = tmdbID
	}
	if p.Subject != "" {
		md["media_title"] = text.TruncateHard(p.Subject, 100)
	}
	if p.Request.RequestedBy != "" {
		md["requested_by"] = text.TruncateHard(p.Request.RequestedBy, 100)
	}
	if p.Issue.IssueType != "" {
		md["issue_type"] = text.TruncateHard(p.Issue.IssueType, 32)
	}
	if p.Issue.ReportedBy != "" {
		md["reported_by"] = text.TruncateHard(p.Issue.ReportedBy, 100)
	}
	if p.Comment.CommentedBy != "" {
		md["commented_by"] = text.TruncateHard(p.Comment.CommentedBy, 100)
	}
	return md
}

// activityKey returns the media type and TMDB id the Live Activity slug is built
// from, or the reason the payload cannot key one.
func (p *overseerrPayload) activityKey() (mediaType, tmdbID string, reason skipReason) {
	switch p.Media.MediaType {
	case "movie", "tv":
	case "":
		return "", "", reasonNoMediaType
	default:
		// The value itself is caller-controlled, so it is logged but not echoed.
		return "", "", reasonBadMediaType
	}

	switch {
	case p.Media.TmdbID == "":
		return "", "", reasonNoTmdbID
	case !isNumeric(p.Media.TmdbID):
		return "", "", reasonBadTmdbID
	}

	return p.Media.MediaType, p.Media.TmdbID, ""
}

func (h *Handler) slug(mediaType, tmdbID string) string {
	return fmt.Sprintf("overseerr-%s-%s", mediaType, tmdbID)
}

func (h *Handler) mapKey(mediaType, tmdbID string) string {
	return fmt.Sprintf("overseerr:%s:%s", mediaType, tmdbID)
}

func (h *Handler) content(p *overseerrPayload, spec eventSpec) pushward.Content {
	total := 4
	c := pushward.Content{
		Template:    pushward.TemplateSteps,
		State:       text.TruncateHard(spec.state, 100),
		Icon:        spec.icon,
		Subtitle:    "Overseerr" + text.SepDot + text.TruncateHard(p.Subject, 50),
		AccentColor: spec.accentColor,
		CurrentStep: &spec.step,
		TotalSteps:  &total,
	}
	if spec.step > 0 {
		c.Progress = float64(spec.step) / float64(total)
	}
	return c
}

// pushOnly answers for a request whose only delivery path was the push, so a
// failed push is the request's failure.
func pushOnly(notifErr error) (*humautil.WebhookResponse, error) {
	if notifErr != nil {
		return nil, humautil.UpstreamError(notifErr)
	}
	return humautil.NewOK(), nil
}

// ignored builds the response for a webhook that was accepted but not acted on
// in full, and records it so the condition is visible in Prometheus rather than
// only in a 200 body.
func ignored(status humautil.Status, reason skipReason) *humautil.WebhookResponse {
	metrics.WebhookIgnoredTotal.WithLabelValues("overseerr", string(status)).Inc()
	return humautil.NewIgnored(status, string(reason))
}

// isAuthFailure reports whether err is the upstream refusing this caller's key,
// as opposed to a transient or content-related failure.
func isAuthFailure(err error) bool {
	var he *pushward.HTTPError
	return errors.As(err, &he) &&
		(he.StatusCode == http.StatusUnauthorized || he.StatusCode == http.StatusForbidden)
}

// collapseID keys the push so that only duplicate deliveries of the SAME
// lifecycle event collapse (including the client's own 5xx retries), while
// distinct lifecycle alerts stay separate. Without a TMDB id it falls back to a
// digest of the subject: a webhook template that omits the media block would
// otherwise give every title the same key, and each new event would replace the
// previous title's push on the lock screen.
func collapseID(subject, label, mediaType, tmdbID string) string {
	if tmdbID == "" {
		return text.SlugHash("overseerr", subject, 4) + "-" + label
	}
	return fmt.Sprintf("overseerr-%s-%s-%s", mediaType, tmdbID, label)
}
