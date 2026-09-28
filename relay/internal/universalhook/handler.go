// Package universalhook serves POST /universal, the relay route for services
// with no handler of their own. A payload is flattened and sent on as one
// plain notification whose title, body and link the proposer picks from it.
// Every event stands alone: nothing about a payload is stored, and nobody is
// asked about one.
package universalhook

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

const provider = "universal"

// viaProposer is universal_events_total's via for a payload sent as a plain
// notification through the proposer's picks.
const viaProposer = "proposer"

// Handler serves the universal webhook.
type Handler struct {
	// store keeps which Live Activities are open. It must hash tenant keys
	// (state.KeyModeStrict): nothing here ever existed under a raw key.
	store    state.Store
	clients  *client.Pool
	config   *config.UniversalConfig
	proposer universal.Proposer
	ender    *lifecycle.Ender
	now      func() time.Time
}

// RegisterRoutes registers POST /universal and returns the Handler. store must
// be wrapped in state.KeyHashing with KeyModeStrict. A nil proposer is the
// heuristic.
func RegisterRoutes(api huma.API, store state.Store, clients *client.Pool, cfg *config.UniversalConfig, proposer universal.Proposer) *Handler {
	if proposer == nil {
		proposer = universal.Heuristic{}
	}
	h := &Handler{
		store:    store,
		clients:  clients,
		config:   cfg,
		proposer: proposer,
		ender: lifecycle.NewEnder(clients, store, provider, lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
		now: time.Now,
	}
	// Hidden: senders reach it as POST / once the root route dispatches here.
	humautil.RegisterWebhook(api, "/universal", "post-universal-webhook",
		"Receive any JSON webhook",
		"Sends an arbitrary JSON payload on as a notification, with a title, body and link picked from the payload.",
		[]string{"Universal"}, h.handleWebhook, humautil.Hidden)
	return h
}

// Ender returns the handler's two-phase ender, for main to flush on shutdown.
func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

type webhookInput struct {
	Source string `query:"source" maxLength:"32" pattern:"^[a-z0-9-]*$" doc:"Names the sending service: its notifications are grouped under it, and one with no title of its own is titled after it"`
	// Read raw: Flatten walks it token by token instead of decoding it
	// whole.
	RawBody []byte
}

// request is one webhook on its way through the handler.
type request struct {
	key       string // the raw hlk_ key, for the client pool and the ender
	source    string
	fields    []universal.Field
	shapes    []universal.ShapeField
	truncated bool
	log       *slog.Logger
}

func (h *Handler) handleWebhook(ctx context.Context, in *webhookInput) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, provider)
	key := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(key), "source", in.Source)

	fields, truncated, err := universal.Flatten(bytes.NewReader(in.RawBody))
	if err != nil {
		metrics.WebhookIgnoredTotal.WithLabelValues(provider, "invalid_json").Inc()
		return nil, huma.Error400BadRequest("body must be a JSON object or array")
	}
	if len(fields) == 0 {
		return ignored(humautil.StatusIgnored, "the payload has no fields"), nil
	}

	r := &request{
		key:       key,
		source:    in.Source,
		fields:    fields,
		shapes:    universal.ShapesOf(fields),
		truncated: truncated,
		log:       log,
	}
	m, via := h.mapping(ctx, r)
	resp, err := h.deliver(ctx, r, m, via)
	if err != nil {
		return nil, humautil.UpstreamError(err)
	}
	return resp, nil
}

// mapping decides how r is delivered, and names where that came from for
// universal_events_total. The payload goes out as a plain notification: the
// proposer picks its title, body and link, and nothing else. Those picks can
// change with each event's values, which is harmless for notifications that
// stand alone, where it would scatter the updates of a card.
func (h *Handler) mapping(ctx context.Context, r *request) (universal.Mapping, string) {
	return h.propose(ctx, r), viaProposer
}

// propose runs the proposer and keeps its title, body and link. universal.
// Fallback never fails; any other proposer that does is replaced by the
// heuristic.
func (h *Handler) propose(ctx context.Context, r *request) universal.Mapping {
	in := universal.NewInput(r.source, r.fields, r.shapes, r.truncated)
	res, err := h.proposer.Propose(ctx, in)
	if err != nil {
		r.log.Warn("proposer failed, using the heuristic", "error", err)
		res, _ = universal.Heuristic{}.Propose(ctx, in)
	}
	p := universal.Proposal{
		Title: res.Proposal.Title,
		Body:  res.Proposal.Body,
		URL:   res.Proposal.URL,
		Kind:  universal.KindNotification,
	}
	return universal.NewMapping(p, r.shapes)
}

// refused reports whether the server refused a call for good: a retry of the
// same event would get the same answer. An unknown key (401), a rate limit or
// a spent quota (429) and a server error are not refusals of this payload.
func refused(err error) bool {
	var he *pushward.HTTPError
	if !errors.As(err, &he) || he.Code == pushward.ErrCodeQuotaExceeded {
		return false
	}
	switch he.StatusCode {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict,
		http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

func ignored(status humautil.Status, detail string) *humautil.WebhookResponse {
	metrics.WebhookIgnoredTotal.WithLabelValues(provider, string(status)).Inc()
	return humautil.NewIgnored(status, detail)
}
