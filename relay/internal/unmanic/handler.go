package unmanic

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path"
	"regexp"

	"github.com/mac-lucky/pushward-integrations/relay/internal/apprise"
	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

var filenameRe = regexp.MustCompile(`(?:Successfully processed|Failed to process): (.+)`)

type Handler struct {
	clients *client.Pool
	config  *config.UnmanicConfig
	ender   *lifecycle.Ender
}

func NewHandler(clients *client.Pool, cfg *config.UnmanicConfig) *Handler {
	return &Handler{
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, nil, "unmanic", lifecycle.EndConfig{
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

	ctx := r.Context()
	ctx = metrics.WithProvider(ctx, "unmanic")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))

	var p apprise.Payload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		slog.Error("failed to decode unmanic payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	var apiErr error
	switch p.Type {
	case "success":
		apiErr = h.handleResult(ctx, userKey, log, p, true)
	case "failure":
		apiErr = h.handleResult(ctx, userKey, log, p, false)
	case "info":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "unmanic"); err != nil {
			log.Error("test notification failed", "provider", "unmanic", "error", err)
		}
	default:
		slog.Debug("unmanic unknown type", "type", p.Type)
	}

	if apiErr != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func parseFilename(message string) string {
	m := filenameRe.FindStringSubmatch(message)
	if len(m) < 2 {
		return "unknown"
	}
	return path.Base(m[1])
}

func slugForFile(filename string) string {
	return text.SlugHash("unmanic", filename, 4)
}

func (h *Handler) handleResult(ctx context.Context, userKey string, log *slog.Logger, p apprise.Payload, success bool) error {
	filename := parseFilename(p.Message)
	slug := slugForFile(filename)

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, filename, h.config.Priority, endedTTL, staleTTL); err != nil {
		log.Error("failed to create unmanic activity", "slug", slug, "error", err)
		return err
	}

	var content pushward.Content
	if success {
		content = pushward.Content{
			Template:    "generic",
			Progress:    1.0,
			State:       "Complete",
			Icon:        "checkmark.circle.fill",
			Subtitle:    "Unmanic \u00b7 " + filename,
			AccentColor: pushward.ColorGreen,
		}
	} else {
		content = pushward.Content{
			Template:    "generic",
			Progress:    1.0,
			State:       "Failed",
			Icon:        "xmark.circle.fill",
			Subtitle:    "Unmanic \u00b7 " + filename,
			AccentColor: pushward.ColorRed,
		}
	}

	h.ender.ScheduleEnd(userKey, slug, slug, content)
	log.Info("unmanic activity", "slug", slug, "type", p.Type, "filename", filename)
	return nil
}
