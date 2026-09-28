package universalhook

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/ratelimit"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
)

// maxFormBytes caps an editor form. The page names fields by index, so even
// a shape at every cap posts a few KiB.
const maxFormBytes = 16 << 10

// Results, as universal_editor_requests_total labels them.
const (
	resultOK          = "ok"
	resultInvalid     = "invalid"
	resultStale       = "stale"
	resultNotFound    = "notfound"
	resultExpired     = "expired"
	resultForbidden   = "forbidden"
	resultRateLimited = "ratelimited"
	resultError       = "error"
)

// keyLimitPattern is the per-key bucket saves share, as ratelimit.AllowKey
// names it.
const keyLimitPattern = "POST /universal/edit"

// Editor serves the mapping editor and list pages. They are plain net/http
// handlers, not huma operations, so every answer is an HTML page, errors
// included. The token in the path is the credential: the pages take no
// integration key, and ignore one sent.
type Editor struct {
	h *Handler
	// origin is the public URL's scheme and host, what the Origin header
	// of the page's own form posts says.
	origin   string
	allowIP  func(ip string) bool
	allowKey func(pattern, key string) bool
}

// RegisterEditor serves GET and POST EditPath{token} and GET ListPath{token}
// on mux, for the universal route h serves. Anything else under those paths
// gets an HTML 404 or 405.
func RegisterEditor(mux *http.ServeMux, h *Handler) *Editor {
	e := &Editor{h: h, allowIP: ratelimit.AllowIP, allowKey: ratelimit.AllowKey}
	if u, err := url.Parse(h.config.PublicURL); err == nil {
		// A browser leaves the scheme's default port out of Origin.
		host := u.Host
		switch u.Scheme {
		case "https":
			host = strings.TrimSuffix(host, ":443")
		case "http":
			host = strings.TrimSuffix(host, ":80")
		}
		e.origin = u.Scheme + "://" + host
	}
	mux.HandleFunc("GET "+EditPath+"{token}", guard(e.view))
	mux.HandleFunc("POST "+EditPath+"{token}", guard(e.save))
	mux.HandleFunc("GET "+ListPath+"{token}", guard(e.list))
	mux.HandleFunc(EditPath, e.other)
	mux.HandleFunc(ListPath, e.other)
	return e
}

// guard answers a panic in a page handler with the fallback page. It logs
// the route pattern, never the path, and cuts the token out of the panic
// value in case it was built from one.
func guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			msg := fmt.Sprint(v)
			if tok := r.PathValue("token"); tok != "" {
				msg = strings.ReplaceAll(msg, tok, "[token]")
			}
			slog.Error("mapping editor panicked", "route", r.Pattern, "panic", msg, "stack", string(debug.Stack()))
			setHeaders(w.Header())
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(fallbackPage))
		}()
		next(w, r)
	}
}

// setHeaders sets what every editor response carries: no caching, no
// referrer (the URL is the credential), no framing, no indexing, and a CSP
// that allows the embedded stylesheet and the form post and nothing else.
func setHeaders(h http.Header) {
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
}

func countEditor(op, result string) {
	metrics.UniversalEditorRequestsTotal.WithLabelValues(op, result).Inc()
}

// fallbackPage is sent when a template fails, which only a bug can cause.
const fallbackPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Error</title></head>` +
	`<body><p>Something went wrong. Try again later.</p></body></html>`

// render executes a page into a buffer first, so a template error sends a
// whole error page rather than half of the one asked for.
func render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, data); err != nil {
		slog.Error("editor page did not render", "page", name, "error", err)
		status = http.StatusInternalServerError
		buf.Reset()
		buf.WriteString(fallbackPage)
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func message(w http.ResponseWriter, status int, m messagePage) {
	m.page = newPage(m.Heading)
	render(w, status, "message.html", m)
}

// The error pages. A token that does not verify gets msgNotFound whatever
// was wrong with it, as review links do.
var (
	msgNotFound = messagePage{
		Heading: "Link not found",
		Text:    "This link is not valid. Open the mapping from the Edit button on its notification, or from the mapping list.",
	}
	msgEditExpired = messagePage{
		Heading: "Link expired",
		Text: "Edit links from a notification work for 30 days, and those on the mapping list for up to 24 hours. " +
			"For a fresh one, send POST /universal/links to the relay with your integration key: " +
			"a notification with a link to the mapping list follows.",
	}
	msgListExpired = messagePage{
		Heading: "Link expired",
		Text: "Links to the mapping list work for 24 hours. For a fresh one, send POST /universal/links " +
			"to the relay with your integration key again.",
	}
	msgGone = messagePage{
		Heading: "Mapping removed",
		Text: "This mapping no longer exists. Mappings are removed after 180 days without a webhook, " +
			"when newer ones push them out, or when a review is left for 7 days. " +
			"The next webhook of this shape proposes a new one.",
	}
	msgForbidden = messagePage{
		Heading: "Not allowed",
		Text:    "This form was sent from another site. Open the mapping from its link and save it there.",
	}
	msgTooLarge  = messagePage{Heading: "Form too large", Text: "The form is larger than the editor accepts. Reload the page and try again."}
	msgMediaType = messagePage{
		Heading: "Unsupported form",
		Text:    "The editor only accepts forms sent as application/x-www-form-urlencoded.",
	}
	msgBadForm     = messagePage{Heading: "Form not understood", Text: "The form was incomplete. Reload the page and try again."}
	msgRateLimited = messagePage{Heading: "Too many requests", Text: "Wait a moment and try again."}
	msgUnavailable = messagePage{Heading: "Try again later", Text: "The mapping store is not answering right now."}
	msgUnreadable  = messagePage{
		Heading: "Cannot edit this mapping",
		Text:    "It was stored by a different version of the relay. Try again in a minute.",
	}
	msgMethod = messagePage{Heading: "Method not allowed", Text: "Open this link in a browser."}
)

// allow spends one request of the client's IP bucket, the one the API
// routes use.
func (e *Editor) allow(w http.ResponseWriter, r *http.Request, op string) bool {
	if e.allowIP(ratelimit.ClientIP(r.RemoteAddr, r.Header.Get)) {
		return true
	}
	if op != "" {
		countEditor(op, resultRateLimited)
	}
	w.Header().Set("Retry-After", "1")
	message(w, http.StatusTooManyRequests, msgRateLimited)
	return false
}

// claims parses the path's token. A token that fails has been answered: 404,
// or 410 for an authentic one past its expiry.
func (e *Editor) claims(w http.ResponseWriter, r *http.Request, op string, scope Scope) (Claims, string, bool) {
	tok := r.PathValue("token")
	c, err := Parse(e.h.key, tok, e.h.now(), scope)
	switch {
	case errors.Is(err, ErrExpired):
		countEditor(op, resultExpired)
		if scope == ScopeList {
			message(w, http.StatusGone, msgListExpired)
		} else {
			message(w, http.StatusGone, msgEditExpired)
		}
		return c, "", false
	case err != nil:
		countEditor(op, resultNotFound)
		message(w, http.StatusNotFound, msgNotFound)
		return c, "", false
	}
	return c, tok, true
}

func logFor(c Claims) *slog.Logger {
	return slog.With("tenant", hex.EncodeToString(c.KeyHash[:4]), "source", c.Source)
}

// load reads the token's row. A row that is gone, or pending past its
// expiry, is a 410: the link was real, the mapping is not there any more.
func (e *Editor) load(w http.ResponseWriter, r *http.Request, op string, c Claims) (*loaded, bool) {
	row, err := e.h.mappings.Get(r.Context(), c.MappingKey())
	if err != nil {
		logFor(c).Warn("mapping editor: store read failed", "error", err)
		countEditor(op, resultError)
		message(w, http.StatusServiceUnavailable, msgUnavailable)
		return nil, false
	}
	if row == nil {
		countEditor(op, resultNotFound)
		message(w, http.StatusGone, msgGone)
		return nil, false
	}
	l, err := load(row, e.h.now())
	if err != nil {
		// Most likely a newer replica wrote it mid-rollout; another replica,
		// or this one once updated, reads it.
		logFor(c).Warn("mapping editor: stored row unreadable", "error", err)
		countEditor(op, resultError)
		w.Header().Set("Retry-After", "30")
		message(w, http.StatusServiceUnavailable, msgUnreadable)
		return nil, false
	}
	return l, true
}

// view is GET EditPath{token}. It never writes.
func (e *Editor) view(w http.ResponseWriter, r *http.Request) {
	setHeaders(w.Header())
	if !e.allow(w, r, opView) {
		return
	}
	c, tok, ok := e.claims(w, r, opView, ScopeEdit)
	if !ok {
		return
	}
	l, ok := e.load(w, r, opView, c)
	if !ok {
		return
	}
	p := l.editorPage(tok, l.draft(), l.row.Version(), nil)
	p.Saved = r.URL.Query().Get("saved") == "1"
	countEditor(opView, resultOK)
	render(w, http.StatusOK, "editor.html", p)
}

// sameOrigin reports whether a form post may have come from the page itself.
// The page's no-referrer policy makes browsers send "Origin: null" on its own
// posts, so null passes like a missing header; Sec-Fetch-Site, which Safari
// sends from 16.4, then still tells the page's own post from another site's.
func (e *Editor) sameOrigin(r *http.Request, log *slog.Logger) bool {
	origin := r.Header.Get("Origin")
	if origin != "" && origin != "null" && !strings.EqualFold(origin, e.origin) {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	if origin == "" || origin == "null" {
		log.Debug("mapping editor: form post without an origin", "origin", origin)
	}
	return true
}

// save is POST EditPath{token}: "Save" stores the form's mapping as
// confirmed, "Send raw instead" rejects the shape and keeps the stored
// mapping. A save that went through redirects back to the page, so a reload
// does not post again.
func (e *Editor) save(w http.ResponseWriter, r *http.Request) {
	setHeaders(w.Header())
	op := opSave
	if !e.allow(w, r, op) {
		return
	}
	c, tok, ok := e.claims(w, r, op, ScopeEdit)
	if !ok {
		return
	}
	log := logFor(c)
	if !e.sameOrigin(r, log) {
		countEditor(op, resultForbidden)
		message(w, http.StatusForbidden, msgForbidden)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
		countEditor(op, resultInvalid)
		message(w, http.StatusUnsupportedMediaType, msgMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		countEditor(op, resultInvalid)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			message(w, http.StatusRequestEntityTooLarge, msgTooLarge)
		} else {
			message(w, http.StatusBadRequest, msgBadForm)
		}
		return
	}
	form := r.PostForm
	op, at, err := readVersion(form)
	if err != nil {
		countEditor(opSave, resultInvalid)
		message(w, http.StatusBadRequest, msgBadForm)
		return
	}
	if !e.allowKey(keyLimitPattern, hex.EncodeToString(c.KeyHash[:])) {
		countEditor(op, resultRateLimited)
		w.Header().Set("Retry-After", "1")
		message(w, http.StatusTooManyRequests, msgRateLimited)
		return
	}
	l, ok := e.load(w, r, op, c)
	if !ok {
		return
	}
	// A form from an earlier incarnation of the row names fields of another
	// shape by index, so it is stale before anything is read from it.
	if l.row.CreatedAt.UnixMicro() != at.CreatedAt.UnixMicro() {
		e.stale(w, op, tok)
		return
	}

	status, mapping := state.MappingRejected, l.row.Mapping
	if op == opSave {
		d, err := l.readDraft(form)
		if err != nil {
			countEditor(op, resultInvalid)
			message(w, http.StatusBadRequest, msgBadForm)
			return
		}
		m, errs := d.mapping(l)
		if len(errs) > 0 {
			countEditor(op, resultInvalid)
			render(w, http.StatusUnprocessableEntity, "editor.html", l.editorPage(tok, d, at, errs))
			return
		}
		if mapping, err = json.Marshal(m); err != nil {
			countEditor(op, resultError)
			message(w, http.StatusInternalServerError, msgUnavailable)
			return
		}
		status = state.MappingConfirmed
	}

	res, err := e.h.mappings.Edit(r.Context(), c.MappingKey(), mapping, status, at, state.DecidedCap(status))
	if err != nil {
		log.Warn("mapping editor: store write failed", "error", err)
		countEditor(op, resultError)
		message(w, http.StatusServiceUnavailable, msgUnavailable)
		return
	}
	if res.Evicted > 0 {
		metrics.UniversalCapHitsTotal.WithLabelValues(string(status)).Add(float64(res.Evicted))
	}
	switch res.Outcome {
	case state.EditOK, state.EditIdempotent:
		countEditor(op, resultOK)
		if res.Outcome == state.EditOK {
			log.Info("mapping edited", "status", status, "rev", res.Rev)
		}
		// Relative, so it resolves behind a path prefix as the form's
		// action does. http.Redirect would make it absolute.
		w.Header().Set("Location", tok+"?saved=1")
		w.WriteHeader(http.StatusSeeOther)
	case state.EditStale:
		e.stale(w, op, tok)
	default:
		countEditor(op, resultNotFound)
		message(w, http.StatusGone, msgGone)
	}
}

func (e *Editor) stale(w http.ResponseWriter, op, tok string) {
	countEditor(op, resultStale)
	message(w, http.StatusConflict, messagePage{
		Heading: "Changed since you opened it",
		Text:    "This mapping was saved or proposed again after the page was loaded. Reload it to see the current version, then make your change again.",
		Link:    &messageLink{Href: tok, Label: "Reload"},
	})
}

// list is GET ListPath{token}: the tenant's mappings, each with an edit link
// of its own.
func (e *Editor) list(w http.ResponseWriter, r *http.Request) {
	setHeaders(w.Header())
	if !e.allow(w, r, opList) {
		return
	}
	c, _, ok := e.claims(w, r, opList, ScopeList)
	if !ok {
		return
	}
	rows, err := e.h.mappings.List(r.Context(), c.KeyHash)
	if err != nil {
		logFor(c).Warn("mapping list: store read failed", "error", err)
		countEditor(opList, resultError)
		message(w, http.StatusServiceUnavailable, msgUnavailable)
		return
	}
	countEditor(opList, resultOK)
	render(w, http.StatusOK, "list.html", e.listPage(rows, c.Expires))
}

var listGroups = []struct {
	status state.MappingStatus
	label  string
}{
	{state.MappingPending, "Waiting for review"},
	{state.MappingConfirmed, "Confirmed"},
	{state.MappingRejected, "Sent raw"},
}

// listPage renders rows. Their edit links expire ListEditTokenTTL from now,
// or with the list link, whichever comes first: a list link is no way to get
// edit links that outlive it.
func (e *Editor) listPage(rows []state.MappingRow, listExpires time.Time) listPage {
	now := e.h.now()
	exp := now.Add(ListEditTokenTTL)
	if listExpires.Before(exp) {
		exp = listExpires
	}
	p := listPage{page: newPage("Webhook mappings")}
	for _, g := range listGroups {
		var out []listRow
		for i := range rows {
			if rows[i].Status == g.status {
				out = append(out, e.listRow(&rows[i], now, exp))
			}
		}
		if len(out) > 0 {
			p.Groups = append(p.Groups, listGroup{Label: g.label, Rows: out})
		}
	}
	return p
}

func (e *Editor) listRow(row *state.MappingRow, now, exp time.Time) listRow {
	lr := listRow{Source: row.Source, Used: used(now, row.LastUsedAt), Kind: "unreadable", Title: "no title field"}
	if lr.Source == "" {
		lr.Source = "no source"
	}
	var m universal.Mapping
	if json.Unmarshal(row.Mapping, &m) == nil {
		if k, ok := kindNames[m.Kind]; ok {
			lr.Kind = k
		}
		if p := m.Paths[universal.RoleTitle]; p != "" {
			lr.Title = "title: " + tailRunes(p, optionPathRunes)
		}
	}
	tok, err := Mint(e.h.key, Claims{
		Scope: ScopeEdit, Expires: exp,
		KeyHash: row.KeyHash, Fingerprint: row.Fingerprint, Source: row.Source,
	})
	if err == nil {
		// Relative to the list page, like the form's action.
		lr.Href = "../edit/" + tok
	}
	return lr
}

// used says how long ago a mapping last delivered a webhook. last_used_at is
// written at most hourly, so days are as precise as it gets.
func used(now, t time.Time) string {
	switch days := int(now.Sub(t) / (24 * time.Hour)); {
	case days <= 0:
		return "used today"
	case days == 1:
		return "used yesterday"
	default:
		return "used " + strconv.Itoa(days) + " days ago"
	}
}

// other answers what the three routes do not: another method on a token
// path gets 405, any other path under them 404.
func (e *Editor) other(w http.ResponseWriter, r *http.Request) {
	setHeaders(w.Header())
	if !e.allow(w, r, "") {
		return
	}
	allowed := "GET, HEAD"
	rest, ok := strings.CutPrefix(r.URL.Path, EditPath)
	if ok {
		allowed = "GET, HEAD, POST"
	} else {
		rest, _ = strings.CutPrefix(r.URL.Path, ListPath)
	}
	if rest == "" || strings.Contains(rest, "/") {
		message(w, http.StatusNotFound, msgNotFound)
		return
	}
	w.Header().Set("Allow", allowed)
	message(w, http.StatusMethodNotAllowed, msgMethod)
}
