package bazarr

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

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

// Greedy first group handles colons in movie titles.
var messageRe = regexp.MustCompile(
	`^(.+)\s+:\s+(.+?)\s+subtitles\s+(downloaded|upgraded|manually downloaded)\s+from\s+(.+?)\s+with a score of\s+([\d.]+)%\.\s*$`,
)

type Handler struct {
	clients *client.Pool
	config  *config.BazarrConfig
	ender   *lifecycle.Ender
}

func NewHandler(clients *client.Pool, cfg *config.BazarrConfig) *Handler {
	return &Handler{
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, nil, "bazarr", lifecycle.EndConfig{
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
	ctx = metrics.WithProvider(ctx, "bazarr")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))

	var p apprise.Payload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		slog.Error("failed to decode bazarr payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	ev := parseMessage(p.Message)
	if ev == nil {
		// Unrecognized message format → treat as test notification.
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "bazarr"); err != nil {
			log.Error("test notification failed", "provider", "bazarr", "error", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}

	if err := h.handleSubtitle(ctx, userKey, log, ev); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func parseMessage(msg string) *subtitleEvent {
	m := messageRe.FindStringSubmatch(msg)
	if len(m) < 6 {
		return nil
	}
	return &subtitleEvent{
		media:    strings.TrimSpace(m[1]),
		language: strings.TrimSpace(m[2]),
		action:   m[3],
		provider: m[4],
		score:    m[5],
	}
}

func (h *Handler) handleSubtitle(ctx context.Context, userKey string, log *slog.Logger, ev *subtitleEvent) error {
	slug := text.SlugHash("bazarr", ev.media, 4)

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, ev.media, h.config.Priority, endedTTL, staleTTL); err != nil {
		log.Error("failed to create bazarr activity", "slug", slug, "error", err)
		return err
	}

	state := "Downloaded"
	if ev.action == "upgraded" {
		state = "Upgraded"
	}

	content := pushward.Content{
		Template:    "generic",
		Progress:    1.0,
		State:       state,
		Icon:        "mdi:download",
		Subtitle:    "Bazarr \u00b7 " + ev.language + " \u00b7 " + ev.score + "% from " + ev.provider,
		AccentColor: pushward.ColorGreen,
	}

	h.ender.ScheduleEnd(userKey, slug, slug, content)
	log.Info("bazarr activity", "slug", slug, "media", ev.media, "language", ev.language, "action", ev.action)
	return nil
}
