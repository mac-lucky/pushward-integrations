package backrest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

var stepLabels = []string{"Running", "Done"}

type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.BackrestConfig
	ender   *lifecycle.Ender
}

func NewHandler(store state.Store, clients *client.Pool, cfg *config.BackrestConfig) *Handler {
	return &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "backrest", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
	}
}

func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var payload webhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		slog.Error("failed to decode webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	userKey := auth.KeyFromContext(r.Context())
	log := slog.With("tenant", auth.KeyHash(userKey))
	ctx := r.Context()
	ctx = metrics.WithProvider(ctx, "backrest")

	var err error
	switch payload.Event {
	case "CONDITION_SNAPSHOT_START":
		err = h.handleStart(ctx, userKey, log, &payload, "Backing up...")
	case "CONDITION_SNAPSHOT_SUCCESS":
		stateText := "Complete · " + formatBytes(payload.DataAdded)
		err = h.handleEnd(ctx, userKey, log, &payload, stateText, pushward.ColorGreen, "checkmark.circle.fill")
	case "CONDITION_SNAPSHOT_WARNING":
		err = h.handleEnd(ctx, userKey, log, &payload, "Complete (warnings)", pushward.ColorOrange, "exclamationmark.triangle.fill")
	case "CONDITION_SNAPSHOT_ERROR":
		stateText := "Failed"
		if payload.Error != "" {
			stateText = "Failed: " + text.TruncateHard(payload.Error, 50)
		}
		err = h.handleEnd(ctx, userKey, log, &payload, stateText, pushward.ColorRed, "xmark.circle.fill")
	case "CONDITION_PRUNE_START":
		err = h.handleStart(ctx, userKey, log, &payload, "Pruning...")
	case "CONDITION_PRUNE_SUCCESS":
		err = h.handleEnd(ctx, userKey, log, &payload, "Pruned", pushward.ColorGreen, "checkmark.circle.fill")
	case "CONDITION_PRUNE_ERROR":
		err = h.handleEnd(ctx, userKey, log, &payload, "Prune Failed", pushward.ColorRed, "xmark.circle.fill")
	case "CONDITION_CHECK_START":
		err = h.handleStart(ctx, userKey, log, &payload, "Checking...")
	case "CONDITION_CHECK_SUCCESS":
		err = h.handleEnd(ctx, userKey, log, &payload, "Check Passed", pushward.ColorGreen, "checkmark.circle.fill")
	case "CONDITION_CHECK_ERROR":
		err = h.handleEnd(ctx, userKey, log, &payload, "Check Failed", pushward.ColorRed, "xmark.circle.fill")
	case "CONDITION_FORGET_START":
		err = h.handleStart(ctx, userKey, log, &payload, "Forgetting...")
	case "CONDITION_FORGET_SUCCESS":
		err = h.handleEnd(ctx, userKey, log, &payload, "Forgotten", pushward.ColorGreen, "checkmark.circle.fill")
	case "CONDITION_FORGET_ERROR":
		err = h.handleEnd(ctx, userKey, log, &payload, "Forget Failed", pushward.ColorRed, "xmark.circle.fill")
	case "CONDITION_ANY_ERROR":
		err = h.handleAlert(ctx, userKey, log, &payload, "Error", pushward.ColorRed, "critical")
	case "CONDITION_SNAPSHOT_SKIPPED":
		err = h.handleAlert(ctx, userKey, log, &payload, "Snapshot Skipped", pushward.ColorBlue, "info")
	default:
		slog.Debug("unknown backrest event", "event", payload.Event)
	}

	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) slugAndKey(p *webhookPayload) (string, string) {
	slug := text.SlugHash("backrest", p.Plan+p.Repo, 4)
	mapKey := fmt.Sprintf("backrest:%s:%s", p.Plan, p.Repo)
	return slug, mapKey
}

func (h *Handler) subtitle(p *webhookPayload) string {
	subtitle := "Backrest"
	if p.Plan != "" {
		subtitle += " · " + text.TruncateHard(p.Plan, 50)
	}
	if p.Repo != "" {
		subtitle += " · " + text.TruncateHard(p.Repo, 50)
	}
	return subtitle
}

func (h *Handler) createActivity(ctx context.Context, userKey string, log *slog.Logger, slug string, p *webhookPayload) (*pushward.Client, error) {
	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	name := text.TruncateHard(p.Plan, 100)
	if name == "" {
		name = "Backup"
	}

	if err := cl.CreateActivity(ctx, slug, name, h.config.Priority, endedTTL, staleTTL); err != nil {
		log.Error("failed to create backrest activity", "slug", slug, "error", err)
		return nil, err
	}
	return cl, nil
}

func (h *Handler) handleStart(ctx context.Context, userKey string, log *slog.Logger, p *webhookPayload, stateText string) error {
	slug, mapKey := h.slugAndKey(p)

	cl, err := h.createActivity(ctx, userKey, log, slug, p)
	if err != nil {
		return err
	}

	step := 1
	total := 2
	content := pushward.Content{
		Template:    "steps",
		Progress:    0,
		State:       stateText,
		Icon:        "arrow.triangle.2.circlepath",
		Subtitle:    h.subtitle(p),
		AccentColor: pushward.ColorBlue,
		CurrentStep: &step,
		TotalSteps:  &total,
		StepLabels:  stepLabels,
	}

	req := pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update backrest activity", "slug", slug, "error", err)
		return err
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "backrest", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		log.Warn("state store write failed", "error", err, "provider", "backrest", "slug", slug)
	}

	log.Info("backrest started", "slug", slug, "event", p.Event, "state", stateText)
	return nil
}

func (h *Handler) handleEnd(ctx context.Context, userKey string, log *slog.Logger, p *webhookPayload, stateText, color, icon string) error {
	slug, mapKey := h.slugAndKey(p)

	if _, err := h.createActivity(ctx, userKey, log, slug, p); err != nil {
		return err
	}

	step := 2
	total := 2
	content := pushward.Content{
		Template:    "steps",
		Progress:    1.0,
		State:       stateText,
		Icon:        icon,
		Subtitle:    h.subtitle(p),
		AccentColor: color,
		CurrentStep: &step,
		TotalSteps:  &total,
		StepLabels:  stepLabels,
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "backrest", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		log.Warn("state store write failed", "error", err, "provider", "backrest", "slug", slug)
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	log.Info("backrest end scheduled", "slug", slug, "event", p.Event, "state", stateText)
	return nil
}

func (h *Handler) handleAlert(ctx context.Context, userKey string, log *slog.Logger, p *webhookPayload, stateText, color, severity string) error {
	slug := text.SlugHash("backrest-alert", p.Plan+p.Repo+p.Event, 4)
	mapKey := fmt.Sprintf("backrest:alert:%s:%s:%s", p.Plan, p.Repo, p.Event)

	cl, err := h.createActivity(ctx, userKey, log, slug, p)
	if err != nil {
		return err
	}

	state := stateText
	if p.Error != "" {
		state = text.TruncateHard(p.Error, 60)
	}

	icon := "exclamationmark.triangle.fill"
	if severity == "info" {
		icon = "info.circle.fill"
	}

	content := pushward.Content{
		Template:    "alert",
		Progress:    1.0,
		State:       state,
		Icon:        icon,
		Subtitle:    h.subtitle(p),
		AccentColor: color,
		Severity:    severity,
	}

	req := pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update backrest alert activity", "slug", slug, "error", err)
		return err
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	log.Info("backrest alert", "slug", slug, "event", p.Event, "state", state)
	return nil
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
