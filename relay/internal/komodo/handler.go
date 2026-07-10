package komodo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
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
	config  *config.KomodoConfig
	ender   *lifecycle.Ender
}

// RegisterRoutes registers the Komodo webhook endpoint and returns the Handler.
func RegisterRoutes(api huma.API, store state.Store, clients *client.Pool, cfg *config.KomodoConfig) *Handler {
	h := &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "komodo", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
	}
	humautil.RegisterWebhook(api, "/komodo", "post-komodo-webhook",
		"Receive Komodo alert webhook",
		"Processes Komodo Custom-alerter events (resolvable server alerts and one-shot notifications).",
		[]string{"Komodo"}, h.handleWebhook)
	return h
}

func (h *Handler) Ender() *lifecycle.Ender { return h.ender }

func (h *Handler) handleWebhook(ctx context.Context, input *struct {
	Body komodoPayload
},
) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, "komodo")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	pwClient := h.clients.Get(userKey)
	p := &input.Body
	kd := &p.Data

	var err error
	switch {
	case kd.Type == "Test":
		if terr := selftest.SendTest(ctx, pwClient, "komodo"); terr != nil {
			log.Error("test notification failed", "provider", "komodo", "error", terr)
		}
	case resolvableTypes[kd.Type]:
		err = h.handleResolvable(ctx, userKey, log, pwClient, p)
	default:
		if kd.Type == "" {
			log.Warn("komodo alert with empty data type", "target", p.Target.ID)
		}
		err = h.handleOneShot(ctx, userKey, log, pwClient, p)
	}

	if err != nil {
		return nil, huma.Error502BadGateway("upstream API error")
	}
	return humautil.NewOK(), nil
}

// handleResolvable maps a resolvable Komodo condition to a Live Activity, adding
// an active alert on trigger and a two-phase end on resolve. Mirrors the Uptime
// Kuma / Gatus down-then-up pattern.
func (h *Handler) handleResolvable(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, p *komodoPayload) error {
	slug, mapKey := slugAndKey(p)
	ov := overrides.FromContext(ctx)

	if p.Resolved {
		return h.handleResolved(ctx, userKey, log, pwClient, p, slug, mapKey)
	}

	// Cancel any pending end from a prior resolve so ENDED can't land after this
	// new ONGOING update.
	h.ender.StopTimer(userKey, mapKey)

	// Degrade to best-effort delivery on store errors: dropping a new alert on a
	// transient DB blip is worse than a possible duplicate, and Komodo has no
	// alert retry queue.
	existing, err := h.store.Get(ctx, "komodo", userKey, mapKey, "")
	if err != nil {
		log.Error("failed to check state, treating alert as new", "target", p.Target.ID, "error", err)
		existing = nil
	}
	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "komodo", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		log.Error("failed to store state, continuing", "target", p.Target.ID, "error", err)
	}

	name := resourceName(p)
	isNew := existing == nil
	// channels=notification suppresses the Live Activity; the isNew notification
	// below still fires so the alert reaches the user as a one-shot.
	if isNew && ov.AllowsActivity() {
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pwClient.CreateActivity(ctx, slug, text.TruncateHard(name, 100), ov.PriorityOr(h.config.Priority), endedTTL, staleTTL); err != nil {
			log.Error("failed to create activity", "slug", slug, "error", err)
			if derr := h.store.Delete(ctx, "komodo", userKey, mapKey, ""); derr != nil {
				log.Warn("state store delete failed", "error", derr, "provider", "komodo", "slug", slug)
			}
			return err
		}
	}

	stateText := summarize(&p.Data)
	if ov.AllowsActivity() {
		color, severity := levelStyle(p.Level)
		req := pushward.UpdateRequest{
			State: pushward.StateOngoing,
			Content: pushward.Content{
				Template:    pushward.TemplateAlert,
				Progress:    1.0,
				State:       stateText,
				Icon:        "exclamationmark.triangle.fill",
				Subtitle:    subtitle(name),
				AccentColor: color,
				Severity:    severity,
				FiredAt:     firedAt(p.TS),
			},
		}
		if err := pwClient.UpdateActivity(ctx, slug, req); err != nil {
			log.Error("failed to update activity", "slug", slug, "error", err)
			// Roll back a brand-new alert's dedup row so a retry re-seeds and
			// re-sends the isNew-gated notification instead of being suppressed.
			if isNew {
				if derr := h.store.Delete(ctx, "komodo", userKey, mapKey, ""); derr != nil {
					log.Warn("state store delete failed", "error", derr, "provider", "komodo", "slug", slug)
				}
			}
			return err
		}
	}

	if isNew && ov.AllowsNotification() {
		notif := h.notification(p, name, slug)
		notif.Body = name + " \u00b7 " + stateText
		notif.Level = ov.LevelOr(pushward.LevelActive)
		if err := pwClient.SendNotification(ctx, notif); err != nil {
			log.Error("failed to send notification", "slug", slug, "error", err)
		}
	}
	log.Info("komodo alert", "slug", slug, "type", p.Data.Type, "level", p.Level)
	return nil
}

func (h *Handler) handleResolved(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, p *komodoPayload, slug, mapKey string) error {
	existing, err := h.store.Get(ctx, "komodo", userKey, mapKey, "")
	if err != nil {
		log.Error("failed to check state", "target", p.Target.ID, "error", err)
		return nil
	}
	if existing == nil {
		return nil // No prior active alert - skip.
	}

	name := resourceName(p)
	ov := overrides.FromContext(ctx)

	// channels=notification never touches the activity; the resolved
	// notification below still fires (channels permitting).
	if ov.AllowsActivity() {
		// The carried err/summary is stale on a resolve event, so render a plain
		// "Resolved" and never surface the original error.
		content := pushward.Content{
			Template:    pushward.TemplateAlert,
			Progress:    1.0,
			State:       "Resolved",
			Icon:        "checkmark.circle.fill",
			Subtitle:    subtitle(name),
			AccentColor: pushward.ColorGreen,
			Severity:    "info",
		}
		h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	} else {
		// ScheduleEnd normally clears the dedup row after the two-phase end.
		// With the activity suppressed it never runs, so drop the row here or
		// the next alert within the stale timeout would be deduped into silence.
		if derr := h.store.Delete(ctx, "komodo", userKey, mapKey, ""); derr != nil {
			log.Warn("state store delete failed", "error", derr, "provider", "komodo", "slug", slug)
		}
	}

	if ov.AllowsNotification() {
		notif := h.notification(p, name, slug)
		notif.Body = "Resolved \u00b7 " + name
		notif.Level = ov.LevelOr(pushward.LevelPassive)
		if err := pwClient.SendNotification(ctx, notif); err != nil {
			log.Error("failed to send notification", "slug", slug, "error", err)
		}
	}
	log.Info("komodo alert resolved", "slug", slug, "type", p.Data.Type)
	return nil
}

// handleOneShot maps a non-resolvable Komodo event to a single push
// notification. Levels map OK -> passive, WARNING -> active, CRITICAL ->
// time-sensitive.
func (h *Handler) handleOneShot(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, p *komodoPayload) error {
	ov := overrides.FromContext(ctx)
	// A one-shot has no Live Activity to keep, so channels=activity leaves
	// nothing to deliver.
	if !ov.AllowsNotification() {
		return nil
	}
	name := resourceName(p)
	notif := h.notification(p, name, oneShotCollapseID(p))
	notif.Body = summarize(&p.Data)
	notif.Level = ov.LevelOr(oneShotLevel(p.Level))
	if err := pwClient.SendNotification(ctx, notif); err != nil {
		log.Error("failed to send notification", "type", p.Data.Type, "error", err)
		return err
	}
	log.Info("komodo notification", "type", p.Data.Type, "level", p.Level)
	return nil
}

func (h *Handler) notification(p *komodoPayload, name, collapseID string) pushward.SendNotificationRequest {
	return pushward.SendNotificationRequest{
		Title:      text.TruncateHard(name, 100),
		Subtitle:   subtitle(name),
		ThreadID:   "komodo",
		CollapseID: collapseID,
		Source:     "komodo",
		Push:       pushward.BoolPtr(true),
		Metadata: map[string]string{
			"alert_type":  p.Data.Type,
			"target_type": p.Target.Type,
			"target_id":   p.Target.ID,
			"level":       p.Level,
		},
	}
}

// slugAndKey derives a stable slug and state key from the alert condition
// (target + data type), not the alert _id, so a resolve event collapses onto
// the same activity as its trigger.
func slugAndKey(p *komodoPayload) (slug, mapKey string) {
	cond := p.Target.Type + "/" + p.Target.ID + "/" + p.Data.Type
	slug = text.SlugHash("komodo", cond, 6)
	mapKey = "komodo:" + cond
	return
}

func oneShotCollapseID(p *komodoPayload) string {
	cond := p.Target.Type + "/" + p.Target.ID + "/" + p.Data.Type + "/" + strconv.FormatInt(p.TS, 10)
	return text.SlugHash("komodo", cond, 6)
}

func resourceName(p *komodoPayload) string {
	switch {
	case p.Data.Data.Name != "":
		return p.Data.Data.Name
	case p.Data.Data.ID != "":
		return p.Data.Data.ID
	case p.Target.ID != "":
		return p.Target.ID
	default:
		return "Komodo"
	}
}

func subtitle(name string) string {
	return "Komodo \u00b7 " + text.TruncateHard(name, 50)
}

func firedAt(tsMillis int64) *int64 {
	if tsMillis <= 0 {
		return nil
	}
	return pushward.Int64Ptr(tsMillis / 1000)
}

func levelStyle(level string) (color, severity string) {
	switch level {
	case "CRITICAL":
		return pushward.ColorRed, "critical"
	case "WARNING":
		return pushward.ColorOrange, "warning"
	default:
		return pushward.ColorBlue, "info"
	}
}

func oneShotLevel(level string) string {
	switch level {
	case "CRITICAL":
		return pushward.LevelTimeSensitive
	case "WARNING":
		return pushward.LevelActive
	default:
		return pushward.LevelPassive
	}
}

// summarize renders a short human state string for a Komodo alert variant.
func summarize(kd *komodoData) string {
	d := kd.Data
	switch kd.Type {
	case "ServerUnreachable":
		return "Unreachable"
	case "ServerCpu":
		if d.Percentage != nil {
			return fmt.Sprintf("CPU %.0f%%", *d.Percentage)
		}
		return "High CPU"
	case "ServerMem":
		if d.UsedGB != nil && d.TotalGB != nil {
			return fmt.Sprintf("Memory %.1f/%.1f GB", *d.UsedGB, *d.TotalGB)
		}
		return "High memory"
	case "ServerDisk":
		if d.UsedGB != nil && d.TotalGB != nil {
			return fmt.Sprintf("Disk %.1f/%.1f GB", *d.UsedGB, *d.TotalGB)
		}
		return "Low disk"
	case "ServerVersionMismatch":
		return "Version mismatch"
	case "SwarmUnhealthy":
		return "Swarm unhealthy"
	case "ContainerStateChange", "StackStateChange":
		if d.From != "" && d.To != "" {
			return d.From + " -> " + d.To
		}
		return "State changed"
	case "DeploymentImageUpdateAvailable", "StackImageUpdateAvailable":
		return "Image update available"
	case "DeploymentAutoUpdated", "StackAutoUpdated":
		return "Auto-updated"
	case "BuildFailed", "RepoBuildFailed":
		return "Build failed"
	case "ProcedureFailed":
		return "Procedure failed"
	case "ActionFailed":
		return "Action failed"
	case "ResourceSyncPendingUpdates":
		return "Pending sync updates"
	case "ScheduleRun":
		return "Scheduled run"
	case "Custom":
		if d.Message != "" {
			return text.Truncate(d.Message, 120)
		}
		return "Custom alert"
	default:
		if kd.Type != "" {
			return kd.Type
		}
		return "Alert"
	}
}
