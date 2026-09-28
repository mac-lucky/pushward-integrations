package universalhook

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/time/rate"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lrumap"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// LinksPath asks for a notification that opens the mapping list.
const LinksPath = "/universal/links"

// A tenant gets one list link a minute from each replica. The limiters sit in
// a bounded LRU, so a flood of keys evicts old ones instead of growing it, and
// SweepLinkLimiters drops the idle ones. The route takes no body.
const (
	linkInterval      = time.Minute
	maxLinkLimiters   = 10_000
	maxLinksBodyBytes = 1024
)

// titleOpen never changes, for the same reason as the review action titles.
const titleOpen = "Open editor"

func newLinkLimiters() *lrumap.Map[*rate.Limiter] {
	return lrumap.New[*rate.Limiter](maxLinkLimiters)
}

// SweepLinkLimiters drops the links limiters idle for longer than
// linkInterval, by when each is full again and no different from a new one.
// It returns how many it dropped.
func (h *Handler) SweepLinkLimiters() int {
	return h.links.Sweep(linkInterval)
}

type linksOutput struct {
	Body struct {
		Status string `json:"status" example:"sent" doc:"sent: the notification went out"`
	}
}

func registerLinks(api huma.API, h *Handler) {
	humautil.RegisterWebhook(api, LinksPath, "post-universal-links",
		"Send a link to the mapping editor",
		"Sends a notification whose action opens the list of this key's universal webhook mappings, valid for 24 hours.",
		[]string{"Universal"}, h.handleLinks, humautil.Hidden, func(op *huma.Operation) {
			op.DefaultStatus = http.StatusAccepted
			op.MaxBodyBytes = maxLinksBodyBytes
		})
}

// handleLinks is POST /universal/links: a way back into the editor for a user
// with no notification left to tap.
func (h *Handler) handleLinks(ctx context.Context, _ *struct{}) (*linksOutput, error) {
	ctx = metrics.WithProvider(ctx, provider)
	key := auth.KeyFromContext(ctx)
	digest := auth.UniversalDigest(key)
	log := slog.With("tenant", hex.EncodeToString(digest[:4]))

	lim := h.links.GetOrCreate(hex.EncodeToString(digest[:]), func() *rate.Limiter {
		return rate.NewLimiter(rate.Every(linkInterval), 1)
	})
	// Reserved rather than spent: a send that fails gives the minute back,
	// so an upstream error does not lock the tenant out.
	now := h.now()
	res := lim.ReserveN(now, 1)
	if !res.OK() || res.DelayFrom(now) > 0 {
		res.CancelAt(now)
		return nil, huma.ErrorWithHeaders(huma.Error429TooManyRequests("a link was sent less than a minute ago"),
			http.Header{"Retry-After": []string{"60"}})
	}

	tok, err := Mint(h.key, Claims{Scope: ScopeList, Expires: now.Add(ListTokenTTL), KeyHash: digest})
	if err != nil {
		res.CancelAt(now)
		return nil, huma.Error500InternalServerError("link not minted")
	}
	req := pushward.SendNotificationRequest{
		Title:      "Webhook mappings",
		Body:       "Review or change how the relay maps your webhooks. The link works for 24 hours.",
		ThreadID:   "universal-review",
		CollapseID: "universal-links",
		Level:      pushward.LevelPassive,
		Actions: []pushward.NotificationAction{{
			ID:                     "open",
			Title:                  titleOpen,
			URL:                    h.config.PublicURL + ListPath + tok,
			Foreground:             true,
			AuthenticationRequired: true,
		}},
		Push: pushward.BoolPtr(true),
	}
	if err := h.clients.SendNotification(ctx, key, log, req); err != nil {
		// At the reservation's own time: rate cancels nothing whose time
		// to act has passed.
		res.CancelAt(now)
		return nil, humautil.UpstreamError(err)
	}
	log.Info("mapping list link sent")
	out := &linksOutput{}
	out.Body.Status = "sent"
	return out, nil
}
