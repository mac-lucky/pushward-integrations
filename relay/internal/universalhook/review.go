package universalhook

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overrides"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// Action titles never change with the mapping. The device registers one
// notification category per distinct action set, and the server keys it on a
// hash that includes the titles, so a title naming the source or the kind
// would register a new category for every mapping.
const (
	titleAccept = "Looks right"
	titleReject = "Send raw instead"
	titleEdit   = "Edit"
)

// maxReviewBytes caps the whole review request. The server refuses a push
// whose APNs payload passes 4096 bytes, and it adds its own envelope around
// what the relay sends.
const maxReviewBytes = 3 << 10

// Review text caps.
const (
	reviewPathRunes  = 48
	reviewValueRunes = 40
	reviewTableRunes = 80
	rawLines         = 6
)

// sendReview asks the user to check a new mapping. It goes out even when
// ?channels= suppresses notifications: channels picks how the webhook's own
// events arrive, and the review is not one of them. Without it the mapping
// could never be confirmed or corrected for a sender that asked for
// activities only. A failed send releases the claim, so a later event
// retries it; it never fails the webhook.
func (h *Handler) sendReview(ctx context.Context, r *request, m universal.Mapping, exp time.Time) {
	req, err := h.reviewRequest(r, m, exp)
	if err == nil {
		err = h.clients.Get(r.key).SendNotification(ctx, req)
	}
	if err != nil {
		r.log.Warn("mapping review not sent", "error", err)
		h.release(ctx, r)
		return
	}
	if err := h.mappings.MarkReviewSent(ctx, r.pk); err != nil {
		r.log.Warn("review sent but not recorded", "error", err)
	}
	r.log.Info("mapping review sent", "kind", m.Kind)
}

func (h *Handler) actionURL(prefix string, s Scope, exp time.Time, pk state.MappingKey) (string, error) {
	tok, err := Mint(h.key, Claims{Scope: s, Expires: exp, KeyHash: pk.KeyHash, Fingerprint: pk.Fingerprint, Source: pk.Source})
	if err != nil {
		return "", err
	}
	return h.config.PublicURL + prefix + tok, nil
}

// editAction opens the editor for a mapping in the browser.
func (h *Handler) editAction(pk state.MappingKey) (pushward.NotificationAction, error) {
	u, err := h.actionURL(EditPath, ScopeEdit, h.now().Add(EditTokenTTL), pk)
	return pushward.NotificationAction{
		ID:                     "edit",
		Title:                  titleEdit,
		URL:                    u,
		Foreground:             true,
		AuthenticationRequired: true,
	}, err
}

// reviewRequest builds the review. Accept and reject are silent POSTs to the
// review route, valid until the pending row expires; edit opens the editor.
func (h *Handler) reviewRequest(r *request, m universal.Mapping, exp time.Time) (pushward.SendNotificationRequest, error) {
	accept, err := h.actionURL(ReviewPath, ScopeAccept, exp, r.pk)
	if err != nil {
		return pushward.SendNotificationRequest{}, err
	}
	reject, err := h.actionURL(ReviewPath, ScopeReject, exp, r.pk)
	if err != nil {
		return pushward.SendNotificationRequest{}, err
	}
	edit, err := h.editAction(r.pk)
	if err != nil {
		return pushward.SendNotificationRequest{}, err
	}

	title := "Check mapping"
	if r.source() != "" {
		title += ": " + r.source()
	}
	req := pushward.SendNotificationRequest{
		Title:      title,
		ThreadID:   "universal-review",
		CollapseID: "ur-" + hex.EncodeToString(r.pk.Fingerprint[:16]),
		Level:      pushward.LevelActive,
		Source:     r.source(),
		Metadata: map[string]string{
			"kind": string(m.Kind),
			"fp":   hex.EncodeToString(r.pk.Fingerprint[:8]),
		},
		Actions: []pushward.NotificationAction{
			{ID: "accept", Title: titleAccept, URL: accept, Method: http.MethodPost, AuthenticationRequired: true},
			{ID: "reject", Title: titleReject, URL: reject, Method: http.MethodPost, Destructive: true, AuthenticationRequired: true},
			edit,
		},
		Push: pushward.BoolPtr(true),
	}
	req.FillSourceDisplayName()
	base, err := json.Marshal(req)
	if err != nil {
		return pushward.SendNotificationRequest{}, err
	}
	req.Body = reviewBody(m, r.fields, r.refused, maxReviewBytes-len(base))
	return req, nil
}

var roleLabels = map[universal.Role]string{
	universal.RoleTitle:       "Title",
	universal.RoleBody:        "Body",
	universal.RoleURL:         "Link",
	universal.RoleCorrelation: "Matched on",
	universal.RoleProgress:    "Progress",
	universal.RoleSeverity:    "Severity",
	universal.RoleLifecycle:   "Status",
}

var kindLabels = map[universal.Kind]string{
	universal.KindNotification: "a notification",
	universal.KindAlert:        "an alert card",
	universal.KindProgress:     "a progress card",
}

// reviewBody says what the mapping did with this payload, one line per mapped
// role, leaving out any line that would take the request past budget bytes of
// JSON. It shows values through Display only, so nothing secret reaches the
// lock screen. A delivery the server refused for good leads the body, since
// the user then sees nothing else from this payload.
func reviewBody(m universal.Mapping, fields []universal.Field, refused string, budget int) string {
	byPath := make(map[string]*universal.Field, len(m.Paths))
	for i := range fields {
		if _, ok := byPath[fields[i].Path]; !ok {
			byPath[fields[i].Path] = &fields[i]
		}
	}

	lead := "Sent as " + kindLabels[m.Kind] + "."
	if refused != "" {
		lead = refused + " Mapped as " + kindLabels[m.Kind] + "."
	}
	lines := []string{lead}
	used := jsonLen(lines[0])
	for _, role := range universal.Roles {
		path := m.Paths[role]
		if path == "" {
			continue
		}
		line := roleLabels[role] + " <- " + tailRunes(path, reviewPathRunes)
		// The correlation value only ever leaves Apply as a hash, so the
		// review names its field and nothing more.
		if f := byPath[path]; f != nil && role != universal.RoleCorrelation {
			if v := universal.Display(f.Path, *f, reviewValueRunes); v != "" {
				line += ` = "` + v + `"`
			}
		}
		switch role {
		case universal.RoleSeverity:
			line += table(m.SeverityValues)
		case universal.RoleLifecycle:
			line += table(m.LifecycleValues)
		}
		// Each further line costs its escaped text plus the escaped newline.
		cost := jsonLen(line) + 2
		if used+cost > budget {
			continue
		}
		lines = append(lines, line)
		used += cost
	}
	return strings.Join(lines, "\n")
}

// jsonLen is the length of s as a JSON string, without the quotes.
func jsonLen(s string) int {
	b, _ := json.Marshal(s)
	return len(b) - 2
}

// table renders a value table, " (firing: ongoing, resolved: ended)", cut
// at reviewTableRunes: the editor shows the whole table.
func table(t map[string]string) string {
	if len(t) == 0 {
		return ""
	}
	var b strings.Builder
	for i, k := range slices.Sorted(maps.Keys(t)) {
		entry := k + ": " + t[k]
		if i > 0 {
			entry = ", " + entry
		}
		if utf8.RuneCountInString(b.String()+entry) > reviewTableRunes {
			b.WriteString(", ...")
			break
		}
		b.WriteString(entry)
	}
	return " (" + b.String() + ")"
}

// tailRunes keeps the end of a long path, which is the part that names the
// field.
func tailRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return "..." + string(r[len(r)-n+3:])
}

// deliverRaw sends a rejected shape as it came: its first fields and their
// display values, and an Edit button to map it after all.
func (h *Handler) deliverRaw(ctx context.Context, r *request) (*humautil.WebhookResponse, error) {
	metrics.UniversalAppliesTotal.WithLabelValues(statusRejected, "raw").Inc()
	ov := overrides.FromContext(ctx)
	if !ov.AllowsNotification() {
		return humautil.NewOK(), nil
	}
	title := r.source()
	if title == "" {
		title = "Webhook"
	}
	req := pushward.SendNotificationRequest{
		Title:    title,
		Source:   r.source(),
		ThreadID: threadID(r.source()),
		Level:    ov.LevelOr(pushward.LevelPassive),
		Push:     pushward.BoolPtr(true),
	}
	if edit, err := h.editAction(r.pk); err == nil {
		req.Actions = []pushward.NotificationAction{edit}
	} else {
		r.log.Warn("edit link not minted", "error", err)
	}
	base, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	req.Body = rawBody(r.fields, maxReviewBytes-len(base))
	if err := h.clients.SendNotification(ctx, r.key, r.log, req); err != nil {
		return nil, humautil.UpstreamError(err)
	}
	return humautil.NewOK(), nil
}

// rawBody lists a payload's first fields and their display values, leaving
// out any line that would take the request past budget bytes of JSON: the
// server refuses a push over 4 KiB, and escaped characters count several
// times over.
func rawBody(fields []universal.Field, budget int) string {
	lines := make([]string, 0, rawLines)
	used := 0
	for _, f := range fields {
		if len(lines) == rawLines {
			break
		}
		v := universal.Display(f.Path, f, sampleRunes)
		if v == "" {
			continue
		}
		line := tailRunes(f.Path, reviewPathRunes) + ": " + v
		cost := jsonLen(line)
		if len(lines) > 0 {
			cost += jsonLen(text.SepDot)
		}
		if used+cost > budget {
			continue
		}
		lines = append(lines, line)
		used += cost
	}
	if len(lines) == 0 {
		return "No values"
	}
	return strings.Join(lines, text.SepDot)
}

type reviewInput struct {
	Token string `path:"token" maxLength:"256"`
}

type reviewOutput struct {
	Body struct {
		Status string `json:"status" doc:"ok, or idempotent when the same decision was already made"`
	}
}

// handleReview is POST /universal/review/{token}: a tap on "Looks right" or
// "Send raw instead". The token is the credential, so it is never logged.
func (h *Handler) handleReview(ctx context.Context, in *reviewInput) (*reviewOutput, error) {
	ctx = metrics.WithProvider(ctx, provider)
	c, err := Parse(h.key, in.Token, h.now(), ScopeAccept, ScopeReject)
	decision, status := "accept", state.MappingConfirmed
	if c.Scope == ScopeReject {
		decision, status = "reject", state.MappingRejected
	}
	count := func(result string) {
		metrics.UniversalReviewsTotal.WithLabelValues(decision, result).Inc()
	}
	switch {
	case errors.Is(err, ErrExpired):
		count("expired")
		return nil, huma.NewError(http.StatusGone, "this review link has expired")
	case err != nil:
		decision = "unknown"
		count("not_found")
		return nil, huma.Error404NotFound("no such review")
	}

	// A pending row answers only to the tokens minted for it. Its expiry is
	// in them, and Decide checks it under the tenant lock, so a link for an
	// earlier proposal of the same shape (decided, evicted, proposed again)
	// cannot decide this one.
	res, err := h.mappings.Decide(ctx, c.MappingKey(), status, c.Expires, state.DecidedCap(status))
	if err != nil {
		count("error")
		return nil, huma.Error503ServiceUnavailable("mapping store unavailable")
	}
	if res.Evicted > 0 {
		metrics.UniversalCapHitsTotal.WithLabelValues(string(status)).Add(float64(res.Evicted))
	}
	switch res.Outcome {
	case state.DecideOK, state.DecideIdempotent:
		count(string(res.Outcome))
		out := &reviewOutput{}
		out.Body.Status = string(res.Outcome)
		return out, nil
	case state.DecideConflict:
		count("conflict")
		return nil, huma.Error409Conflict("the other decision was already made for this mapping")
	}
	count("not_found")
	return nil, huma.Error404NotFound("no such review")
}
