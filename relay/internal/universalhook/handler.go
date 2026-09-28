// Package universalhook serves POST /universal, the relay route for services
// with no handler of their own. A payload is flattened, its shape looked up in
// the tenant's stored mappings and delivered through the mapping found; a shape
// seen for the first time is delivered through a proposed mapping, and the user
// is asked to review it.
package universalhook

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/time/rate"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lrumap"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

const provider = "universal"

// Paths that carry a capability token. Their URLs must stay out of traces
// and logs, since the token is the whole credential. A deployment that ships
// access logs has to redact these prefixes too; a new prefix here needs the
// same change there.
const (
	ReviewPath = "/universal/review/"
	EditPath   = "/universal/edit/"
	ListPath   = "/universal/list/"
)

// IsCapabilityPath reports whether path carries a capability token.
func IsCapabilityPath(path string) bool {
	return strings.HasPrefix(path, ReviewPath) || strings.HasPrefix(path, EditPath) || strings.HasPrefix(path, ListPath)
}

// Handler serves the universal webhook and its review links.
type Handler struct {
	// store keeps which Live Activities are open. It must hash tenant keys
	// (state.KeyModeStrict): nothing here ever existed under a raw key.
	store    state.Store
	mappings state.MappingStore
	clients  *client.Pool
	config   *config.UniversalConfig
	proposer universal.Proposer
	ender    *lifecycle.Ender
	key      []byte
	now      func() time.Time
	// links rate-limits POST /universal/links per tenant.
	links *lrumap.Map[*rate.Limiter]
}

// RegisterRoutes registers POST /universal, the review route and POST
// /universal/links, and returns the Handler; RegisterEditor serves the pages
// the links open. store must be wrapped in state.KeyHashing with KeyModeStrict.
// A nil proposer is the heuristic.
func RegisterRoutes(api huma.API, store state.Store, mappings state.MappingStore, clients *client.Pool, cfg *config.UniversalConfig, proposer universal.Proposer) (*Handler, error) {
	key, err := cfg.ReviewKeyBytes()
	if err != nil {
		return nil, err
	}
	if proposer == nil {
		proposer = universal.Heuristic{}
	}
	h := &Handler{
		store:    store,
		mappings: mappings,
		clients:  clients,
		config:   cfg,
		proposer: proposer,
		ender: lifecycle.NewEnder(clients, store, provider, lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
		key:   key,
		now:   time.Now,
		links: newLinkLimiters(),
	}
	// Hidden: senders reach it as POST / once the root route dispatches here.
	humautil.RegisterWebhook(api, "/universal", "post-universal-webhook",
		"Receive any JSON webhook",
		"Maps an arbitrary JSON payload onto a notification or Live Activity, and asks for a review of each new payload shape.",
		[]string{"Universal"}, h.handleWebhook, humautil.Hidden)
	humautil.RegisterPublic(api, ReviewPath+"{token}", "post-universal-review",
		"Accept or reject a proposed universal mapping", h.handleReview)
	registerLinks(api, h)
	return h, nil
}

// Ender returns the handler's two-phase ender, for main to flush on shutdown.
func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

type webhookInput struct {
	Source string `query:"source" maxLength:"32" pattern:"^[a-z0-9-]*$" doc:"Names the sending service, so its payloads get mappings of their own"`
	// Read raw: Flatten walks it token by token instead of decoding it
	// whole.
	RawBody []byte
}

// request is one webhook on its way through the handler.
type request struct {
	key       string // the raw hlk_ key, for the client pool and the ender
	pk        state.MappingKey
	fields    []universal.Field
	truncated bool
	shapes    []universal.ShapeField // see shapeFields
	log       *slog.Logger
	// refused, when set, is the line the review adds about a delivery the
	// server refused for good.
	refused string
}

func (r *request) source() string { return r.pk.Source }

// shapeFields computes the payload's shapes on first use. Only a proposal
// needs them; a stored mapping is applied to the fields alone.
func (r *request) shapeFields() []universal.ShapeField {
	if r.shapes == nil {
		r.shapes = universal.ShapesOf(r.fields)
	}
	return r.shapes
}

func (h *Handler) handleWebhook(ctx context.Context, in *webhookInput) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, provider)
	key := auth.KeyFromContext(ctx)
	digest := auth.UniversalDigest(key)
	log := slog.With("tenant", hex.EncodeToString(digest[:4]), "source", in.Source)

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
		pk:        state.MappingKey{KeyHash: digest, Source: in.Source, Fingerprint: fingerprint(in.Source, fields)},
		fields:    fields,
		truncated: truncated,
		log:       log,
	}
	resp, err := h.serve(ctx, r)
	if err != nil {
		return nil, humautil.UpstreamError(err)
	}
	return resp, nil
}

func (h *Handler) serve(ctx context.Context, r *request) (*humautil.WebhookResponse, error) {
	row, err := h.mappings.GetForDelivery(ctx, r.pk)
	if err != nil {
		r.log.Error("mapping lookup failed, delivering with a fresh proposal", "error", err)
		return h.unstored(ctx, r, "store_error")
	}
	if row == nil {
		return h.newShape(ctx, r)
	}
	return h.known(ctx, r, row)
}

func fingerprint(source string, fields []universal.Field) [32]byte {
	var fp [32]byte
	// Fingerprint is hex of a SHA-256, so this cannot fail.
	_, _ = hex.Decode(fp[:], []byte(universal.Fingerprint(source, fields)))
	return fp
}

// unstored delivers through a proposal made on the spot and not stored, so
// there is no row for a review to decide. result says why, for
// universal_proposals_total.
func (h *Handler) unstored(ctx context.Context, r *request, result string) (*humautil.WebhookResponse, error) {
	m, _ := h.propose(ctx, r)
	return h.deliverUnstored(ctx, r, m, result)
}

func (h *Handler) deliverUnstored(ctx context.Context, r *request, m universal.Mapping, result string) (*humautil.WebhookResponse, error) {
	metrics.UniversalProposalsTotal.WithLabelValues(string(m.Kind), result).Inc()
	if result == "cap_pending" {
		metrics.UniversalCapHitsTotal.WithLabelValues("pending").Inc()
		r.log.Info("pending mapping cap reached, delivering without a review")
	}
	return h.deliver(ctx, r, m, statusNew)
}

func (h *Handler) newShape(ctx context.Context, r *request) (*humautil.WebhookResponse, error) {
	m, res := h.propose(ctx, r)
	row, err := h.newRow(r, m, res)
	if err != nil {
		r.log.Error("mapping not stored", "error", err)
		return h.deliverUnstored(ctx, r, m, "store_error")
	}

	inserted, err := h.mappings.InsertPending(ctx, row, state.MaxPending)
	switch {
	case errors.Is(err, state.ErrPendingCap):
		// Delivered, never reviewed: it is proposed afresh on its next event,
		// by when an older proposal may have been decided.
		return h.deliverUnstored(ctx, r, m, "cap_pending")
	case err != nil:
		r.log.Error("mapping not stored", "error", err)
		return h.deliverUnstored(ctx, r, m, "store_error")
	case !inserted:
		// Another replica stored this shape first; its mapping, and its
		// review, win.
		metrics.UniversalProposalsTotal.WithLabelValues(string(m.Kind), "deduped").Inc()
		cur, err := h.mappings.GetForDelivery(ctx, r.pk)
		if err != nil || cur == nil {
			return h.deliver(ctx, r, m, statusNew)
		}
		return h.known(ctx, r, cur)
	}

	metrics.UniversalProposalsTotal.WithLabelValues(string(m.Kind), "created").Inc()
	r.log.Info("new universal mapping", "kind", m.Kind, "proposer", res.By)
	return h.deliverProposed(ctx, r, m, *row.ExpiresAt)
}

// replaceStale proposes again for a shape whose stored mapping cannot be read,
// one written by another mapping version, and replaces the row. Leaving it in
// place would deliver every event through an on-the-spot proposal forever,
// never reviewed.
func (h *Handler) replaceStale(ctx context.Context, r *request, staleRev int) (*humautil.WebhookResponse, error) {
	m, res := h.propose(ctx, r)
	row, err := h.newRow(r, m, res)
	if err != nil {
		r.log.Error("mapping not stored", "error", err)
		return h.deliverUnstored(ctx, r, m, "store_error")
	}
	replaced, err := h.mappings.ReplaceStale(ctx, row, staleRev, state.MaxPending)
	switch {
	case errors.Is(err, state.ErrPendingCap):
		return h.deliverUnstored(ctx, r, m, "cap_pending")
	case err != nil:
		r.log.Error("mapping not replaced", "error", err)
		return h.deliverUnstored(ctx, r, m, "store_error")
	case !replaced:
		// Another replica replaced it first.
		return h.deliverUnstored(ctx, r, m, "deduped")
	}
	metrics.UniversalProposalsTotal.WithLabelValues(string(m.Kind), "stale_version").Inc()
	r.log.Info("stale universal mapping proposed again", "kind", m.Kind, "proposer", res.By)
	return h.deliverProposed(ctx, r, m, *row.ExpiresAt)
}

// deliverProposed delivers through a mapping just stored as pending and
// claimed by this request, then sends its review. A delivery the server
// refused for good is still reviewed, with the refusal in it: a retry would
// be refused the same way, and without the review the user would never learn
// the mapping exists. Any other failure keeps the review for the retry.
func (h *Handler) deliverProposed(ctx context.Context, r *request, m universal.Mapping, exp time.Time) (*humautil.WebhookResponse, error) {
	resp, err := h.deliver(ctx, r, m, statusNew)
	if err != nil {
		reason, permanent := refusal(err)
		if !permanent {
			h.release(ctx, r)
			return nil, err
		}
		r.refused = "Delivery was refused (" + reason + ")."
	}
	h.sendReview(ctx, r, m, exp)
	return resp, err
}

// known delivers through a stored mapping.
func (h *Handler) known(ctx context.Context, r *request, row *state.MappingRow) (*humautil.WebhookResponse, error) {
	if row.Status == state.MappingRejected {
		resp, err := h.deliverRaw(ctx, r)
		if err == nil {
			h.touch(ctx, r)
		}
		return resp, err
	}

	var m universal.Mapping
	if err := json.Unmarshal(row.Mapping, &m); err != nil || m.V != universal.MappingVersion {
		r.log.Warn("stored mapping unreadable, proposing again", "error", err, "version", m.V)
		return h.replaceStale(ctx, r, row.Rev)
	}
	status := statusConfirmed
	if row.Status == state.MappingPending {
		status = statusPending
	}
	review := row.Status == state.MappingPending && row.ReviewSentAt == nil && row.ExpiresAt != nil
	resp, err := h.deliver(ctx, r, m, status)
	if err != nil {
		// As in deliverProposed: only a refusal for good is worth a review
		// now, and the event's retry is left to the sender.
		if reason, permanent := refusal(err); permanent && review {
			r.refused = "Delivery was refused (" + reason + ")."
			h.claimReview(ctx, r, m, *row.ExpiresAt)
		}
		return nil, err
	}
	if review {
		h.claimReview(ctx, r, m, *row.ExpiresAt)
	}
	h.touch(ctx, r)
	return resp, nil
}

// refusal reports whether the server refused a call for good, and a short
// reason: the stable error code when it gave one. A retry of the same event
// would get the same answer. An unknown key (401), a rate limit or a spent
// quota (429) and a server error are not refusals of this payload.
func refusal(err error) (string, bool) {
	var he *pushward.HTTPError
	if !errors.As(err, &he) || he.Code == pushward.ErrCodeQuotaExceeded {
		return "", false
	}
	switch he.StatusCode {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict,
		http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
	default:
		return "", false
	}
	if he.Code != "" {
		return he.Code, true
	}
	return fmt.Sprintf("HTTP %d", he.StatusCode), true
}

func (h *Handler) touch(ctx context.Context, r *request) {
	if err := h.mappings.Touch(ctx, r.pk); err != nil {
		r.log.Warn("mapping touch failed", "error", err)
	}
}

// reviewTimeout bounds each review step. The steps run detached from the
// request: a sender that hangs up once its event is delivered must not leave
// the claim held for ReviewClaimTTL or a sent review unrecorded.
const reviewTimeout = 5 * time.Second

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), reviewTimeout)
}

func (h *Handler) release(ctx context.Context, r *request) {
	ctx, cancel := detached(ctx)
	defer cancel()
	if err := h.mappings.ReleaseReview(ctx, r.pk); err != nil {
		r.log.Warn("review claim release failed", "error", err)
	}
}

// claimReview sends the review of a pending row whose review has not gone
// out, if this request wins the claim on it.
func (h *Handler) claimReview(ctx context.Context, r *request, m universal.Mapping, exp time.Time) {
	cctx, cancel := detached(ctx)
	claimed, err := h.mappings.ClaimReview(cctx, r.pk)
	cancel()
	if err != nil {
		r.log.Warn("review claim failed", "error", err)
	}
	if claimed {
		h.sendReview(ctx, r, m, exp)
	}
}

// propose runs the proposer. universal.Fallback never fails; any other
// proposer that does is replaced by the heuristic.
func (h *Handler) propose(ctx context.Context, r *request) (universal.Mapping, universal.Result) {
	in := universal.NewInput(r.source(), r.fields, r.shapeFields(), r.truncated)
	res, err := h.proposer.Propose(ctx, in)
	if err != nil {
		r.log.Warn("proposer failed, using the heuristic", "error", err)
		res, _ = universal.Heuristic{}.Propose(ctx, in)
	}
	return universal.NewMapping(res.Proposal, r.shapeFields()), res
}

// Samples and candidates stored for the review and the editor.
const (
	maxSamples        = 256
	maxSamplesBytes   = 16 << 10
	sampleRunes       = 60
	candidatesPerRole = 5
)

func (h *Handler) newRow(r *request, m universal.Mapping, res universal.Result) (*state.MappingRow, error) {
	cands := make(map[universal.Role][]string, len(universal.Roles))
	keep := make([]string, 0, len(m.Paths)+candidatesPerRole*len(universal.Roles))
	for _, p := range m.Paths {
		keep = append(keep, p)
	}
	for _, role := range universal.Roles {
		for i, c := range universal.RankShapes(r.shapeFields(), role) {
			if i == candidatesPerRole {
				break
			}
			cands[role] = append(cands[role], c.Path)
			keep = append(keep, c.Path)
		}
	}

	mapping, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	// The editor offers the candidates first, so their fields are kept even
	// when the shape is cut to size.
	shape, err := json.Marshal(universal.NewShape(r.shapeFields(), r.truncated, keep...))
	if err != nil {
		return nil, err
	}
	proposal, err := json.Marshal(res.Proposal)
	if err != nil {
		return nil, err
	}
	candidates, err := json.Marshal(cands)
	if err != nil {
		return nil, err
	}
	samples, err := json.Marshal(samplesOf(r.fields, keep))
	if err != nil {
		return nil, err
	}
	exp := h.reviewExpiry()
	return &state.MappingRow{
		MappingKey: r.pk,
		Status:     state.MappingPending,
		Mapping:    mapping,
		Shape:      shape,
		Proposal:   proposal,
		Samples:    samples,
		Candidates: candidates,
		Proposer:   res.By,
		ExpiresAt:  &exp,
	}, nil
}

// reviewExpiry is when a new pending row expires. It is whole seconds, the
// precision of the review tokens that carry it, so a token's expiry equals its
// row's exactly.
func (h *Handler) reviewExpiry() time.Time {
	return time.Unix(h.now().Add(state.PendingTTL).Unix(), 0)
}

// samplesOf renders the display value of each field, the candidates first and
// then the rest in document order, up to maxSamples entries and
// maxSamplesBytes of JSON. Every field is in it, the one mapped as correlation
// included: the editor shows each field it offers with its sample, and Display
// has already redacted what is secret, value-only secrets too. Empty values
// are left out. The store drops samples after state.SamplesTTL.
func samplesOf(fields []universal.Field, first []string) map[string]string {
	byPath := make(map[string]int, len(fields))
	for i := len(fields) - 1; i >= 0; i-- {
		byPath[fields[i].Path] = i
	}
	out := make(map[string]string, min(len(fields), maxSamples))
	size := len("{}")
	add := func(i int) {
		f := fields[i]
		if _, done := out[f.Path]; done || len(out) == maxSamples {
			return
		}
		v := universal.Display(f.Path, f, sampleRunes)
		if v == "" {
			return
		}
		k, _ := json.Marshal(f.Path)
		jv, _ := json.Marshal(v)
		cost := len(k) + len(jv) + 2 // the colon and a comma
		if size+cost > maxSamplesBytes {
			return
		}
		out[f.Path] = v
		size += cost
	}
	for _, p := range first {
		if i, ok := byPath[p]; ok {
			add(i)
		}
	}
	for i := range fields {
		add(i)
	}
	return out
}

func ignored(status humautil.Status, detail string) *humautil.WebhookResponse {
	metrics.WebhookIgnoredTotal.WithLabelValues(provider, string(status)).Inc()
	return humautil.NewIgnored(status, detail)
}
