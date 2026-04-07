package starr

import (
	"context"
	"encoding/json"
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
		apiErr = h.handleApplicationUpdate(ctx, userKey, log, "prowlarr", p)
	default:
		slog.Debug("ignored event", "event_type", envelope.EventType)
	}

	if apiErr != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) handleProwlarrGrab(ctx context.Context, userKey string, log *slog.Logger, p *ProwlarrGrabPayload) error {
	body := "Grabbed · " + p.Release.Indexer
	if p.Source != "" {
		body += " → " + p.Source
	}

	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title:      "Prowlarr",
		Subtitle:   text.Truncate(p.Release.ReleaseTitle, 80),
		Body:       body,
		ThreadID:   "prowlarr",
		CollapseID: text.SlugHash("prowlarr-grab", p.Release.ReleaseTitle, 6),
		Level:      pushward.LevelActive,
		Category:   "grab",
		Source:     "prowlarr",
		Push:       true,
	})
}
