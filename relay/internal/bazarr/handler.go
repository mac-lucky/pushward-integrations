package bazarr

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/apprise"
	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
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
}

// RegisterRoutes registers the Bazarr webhook endpoint and returns the Handler.
func RegisterRoutes(api huma.API, clients *client.Pool, cfg *config.BazarrConfig) *Handler {
	h := &Handler{
		clients: clients,
	}
	humautil.RegisterWebhook(api, "/bazarr", "post-bazarr-webhook",
		"Receive Bazarr subtitle webhook",
		"Processes Bazarr subtitle download events via Apprise notifications.",
		[]string{"Bazarr"}, h.handleWebhook)
	return h
}

func (h *Handler) handleWebhook(ctx context.Context, input *struct {
	Body apprise.Payload
}) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, "bazarr")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))

	ev := parseMessage(input.Body.Message)
	if ev == nil {
		// Unrecognized message format → treat as test notification.
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "bazarr"); err != nil {
			log.Error("test notification failed", "provider", "bazarr", "error", err)
		}
		return humautil.NewOK(), nil
	}

	if err := h.handleSubtitle(ctx, userKey, log, ev); err != nil {
		return nil, huma.Error502BadGateway("upstream API error")
	}
	return humautil.NewOK(), nil
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
	action := "Downloaded"
	if ev.action == "upgraded" {
		action = "Upgraded"
	}

	req := pushward.SendNotificationRequest{
		Title:      action + " · " + ev.language,
		Subtitle:   ev.media,
		Body:       ev.score + "% from " + ev.provider,
		ThreadID:   text.Slug("bazarr-", ev.media),
		CollapseID: text.SlugHash("bazarr", ev.media, 4),
		Level:      pushward.LevelActive,
		Category:   "subtitle-" + strings.ReplaceAll(ev.action, " ", "-"),
		Source:     "bazarr",
		Push:       true,
	}

	if err := h.clients.SendNotification(ctx, userKey, log, req); err != nil {
		return err
	}
	log.Info("bazarr notification", "media", ev.media, "language", ev.language, "action", ev.action)
	return nil
}
