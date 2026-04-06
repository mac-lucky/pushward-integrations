package starr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

func (h *Handler) handleProwlarrWebhook(w http.ResponseWriter, r *http.Request) {
	raw, ok := decodePayload(w, r)
	if !ok {
		return
	}

	var envelope webhookPayload
	if err := json.Unmarshal(raw, &envelope); err != nil {
		slog.Error("failed to decode event type", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	ctx = metrics.WithProvider(ctx, "starr")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))

	var apiErr error
	switch envelope.EventType {
	case "Test":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "prowlarr"); err != nil {
			log.Error("test notification failed", "provider", "prowlarr", "error", err)
		}
	case "Grab":
		p, ok := unmarshalPayload[ProwlarrGrabPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleProwlarrGrab(ctx, userKey, log, p)
	case "Health":
		p, ok := unmarshalPayload[HealthPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleHealth(ctx, userKey, log, "prowlarr", p)
	case "HealthRestored":
		p, ok := unmarshalPayload[HealthRestoredPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleHealthRestored(ctx, userKey, log, "prowlarr", p)
	case "ApplicationUpdate":
		p, ok := unmarshalPayload[ApplicationUpdatePayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleProwlarrApplicationUpdate(ctx, userKey, log, p)
	default:
		slog.Debug("ignored event", "event_type", envelope.EventType)
	}

	if apiErr != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleProwlarrGrab(ctx context.Context, userKey string, log *slog.Logger, p *ProwlarrGrabPayload) error {
	title := text.Truncate(p.Release.ReleaseTitle, 80)
	slug := text.SlugHash("prowlarr-grab", p.Release.ReleaseTitle, 6)

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, title, h.config.Priority, endedTTL, staleTTL); err != nil {
		log.Error("failed to create activity", "slug", slug, "error", err)
		return err
	}

	subtitle := "Prowlarr · " + p.Release.Indexer
	if p.Source != "" {
		subtitle += " → " + p.Source
	}

	content := pushward.Content{
		Template:    "generic",
		Progress:    1.0,
		State:       "Grabbed",
		Icon:        "magnifyingglass",
		Subtitle:    subtitle,
		AccentColor: pushward.ColorBlue,
	}

	h.ender.ScheduleEnd(userKey, "prowlarr:grab:"+slug, slug, content)
	log.Info("grab received", "slug", slug, "indexer", p.Release.Indexer, "trigger", p.Trigger)
	return nil
}

func (h *Handler) handleProwlarrApplicationUpdate(ctx context.Context, userKey string, log *slog.Logger, p *ApplicationUpdatePayload) error {
	slug := text.SlugHash("prowlarr-update", p.NewVersion, 4)

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	name := fmt.Sprintf("Prowlarr %s → %s", p.PreviousVersion, p.NewVersion)
	if err := cl.CreateActivity(ctx, slug, name, h.config.Priority, endedTTL, staleTTL); err != nil {
		log.Error("failed to create activity", "slug", slug, "error", err)
		return err
	}

	content := pushward.Content{
		Template:    "generic",
		Progress:    1.0,
		State:       "Updated",
		Icon:        "arrow.triangle.2.circlepath",
		Subtitle:    "Prowlarr · " + p.NewVersion,
		AccentColor: pushward.ColorGreen,
	}

	h.ender.ScheduleEnd(userKey, "prowlarr:update:"+slug, slug, content)
	log.Info("application update", "slug", slug, "from", p.PreviousVersion, "to", p.NewVersion)
	return nil
}
