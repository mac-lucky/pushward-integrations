// Package universalhook serves POST /universal, the relay route for services
// with no handler of their own. A payload a preset knows is mapped the way
// the preset says, Live Activities included; any other is sent on as one
// plain notification whose title, body and link the proposer picks from it.
// No mapping is stored and nobody is asked about one: each event is mapped on
// its own, and only a preset's open card keeps state, until it ends.
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
	"github.com/mac-lucky/pushward-integrations/relay/internal/universal/presets"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

const provider = "universal"

// universal_events_total's via: a preset's mapping, or the proposer's picks
// for the plain notification a payload no preset knows becomes.
const (
	viaPreset   = "preset"
	viaProposer = "proposer"
)

// Handler serves the universal webhook.
type Handler struct {
	// store keeps which Live Activities are open, under hashed tenant keys
	// (state.KeyHashing).
	store    state.Store
	clients  *client.Pool
	config   *config.UniversalConfig
	proposer universal.Proposer
	ender    *lifecycle.Ender
	now      func() time.Time
}

// RegisterRoutes registers POST /universal and returns the Handler. store must
// be wrapped in state.KeyHashing. A nil proposer is the heuristic.
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
		"Maps a payload from a known service the way its preset says, and sends any other JSON payload on as a notification, with a title, body and link picked from the payload.",
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
	// sendLog is log without "source": the client pool adds the
	// notification's own source, which is the same value.
	sendLog *slog.Logger
}

func (h *Handler) handleWebhook(ctx context.Context, in *webhookInput) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, provider)
	key := auth.KeyFromContext(ctx)
	sendLog := slog.With("tenant", auth.KeyHash(key))
	log := sendLog.With("source", in.Source)

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
		sendLog:   sendLog,
	}
	m, via := h.mapping(ctx, r)
	resp, err := h.deliver(ctx, r, m, via)
	if err != nil {
		return nil, humautil.UpstreamError(err)
	}
	return resp, nil
}

// mapping decides how r is delivered, and names where that came from for
// universal_events_total. A preset maps the payloads of a service it knows,
// Live Activities included: it picks by paths, never by values, so every
// event of a shape maps the same way. Anything else goes out as a plain
// notification whose title, body and link the proposer picks. Those picks can
// change with each event's values, which is harmless for notifications that
// stand alone, where it would scatter the updates of a card.
func (h *Handler) mapping(ctx context.Context, r *request) (universal.Mapping, string) {
	if h.config.Presets {
		if p, m, ok := presets.MatchShapes(r.source, r.fields, r.shapes); ok {
			metrics.UniversalPresetHitsTotal.WithLabelValues(p.ID).Inc()
			return m, viaPreset
		}
	}
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
