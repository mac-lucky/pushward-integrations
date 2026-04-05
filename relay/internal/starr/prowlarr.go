package starr

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
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
	case "Health":
		var p HealthPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Error("failed to decode health payload", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		apiErr = h.handleHealth(ctx, userKey, log, "prowlarr", &p)
	case "HealthRestored":
		var p HealthRestoredPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Error("failed to decode health restored payload", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		apiErr = h.handleHealthRestored(ctx, userKey, log, "prowlarr", &p)
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
