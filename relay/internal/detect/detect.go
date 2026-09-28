// Package detect tells which of the relay's providers sent a webhook, from its
// headers and the top level of its JSON body. It errs towards no match: a
// payload it does not claim goes to the universal route, which handles
// anything, while a wrong claim hands the payload to a handler that drops or
// garbles it.
package detect

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// How a Match was made.
const (
	// ViaHeader is a sender that names itself in its headers.
	ViaHeader = "header"
	// ViaBody is a body signature.
	ViaBody = "body"
	// ViaVeto is a sender the headers identify as one with no route here. It
	// comes back with ok false, and the body is not looked at.
	ViaVeto = "veto"
)

// Match is the route a webhook belongs on, and the rule that said so. Source
// names the sender of a vetoed webhook when the headers tell: the universal
// route then reads it as the ?source= the sender did not set.
type Match struct {
	Route  string
	Rule   string
	Via    string
	Source string
}

// Detect returns the route of the provider that sent a webhook with headers h
// and body body. ok is false when no provider claims it; the Match then
// carries the veto rule, if a header rule turned the body rules off.
func Detect(h http.Header, body []byte) (m Match, ok bool) {
	v := &view{body: body}
	if m, ok, decided := byHeader(h, v); decided {
		return m, ok
	}
	for _, r := range bodyRules {
		if r.match(v) {
			return Match{Route: r.route, Rule: r.name, Via: ViaBody}, true
		}
	}
	return Match{}, false
}

// Routes lists every route Detect can return.
func Routes() []string {
	out := []string{"/radarr", "/sonarr", "/prowlarr", "/gitea", "/forgejo"}
	for _, r := range bodyRules {
		out = append(out, r.route)
	}
	return out
}

var starrAgents = []struct{ prefix, route string }{
	{"Radarr/", "/radarr"},
	{"Sonarr/", "/sonarr"},
	{"Prowlarr/", "/prowlarr"},
}

// otherStarr are the *arr apps with no route here. Their webhooks look like
// Radarr's closely enough to fool a body rule written for one.
var otherStarr = []string{"Lidarr/", "Readarr/", "Whisparr/"}

// byHeader applies the header rules. decided is false when none applies and
// the body rules should run.
func byHeader(h http.Header, v *view) (m Match, ok, decided bool) {
	ua := h.Get("User-Agent")
	for _, a := range starrAgents {
		if strings.HasPrefix(ua, a.prefix) {
			if v.isString("eventType") {
				return Match{Route: a.route, Rule: agentName(a.prefix), Via: ViaHeader}, true, true
			}
			// Radarr's other connections (Apprise, ntfy, Gotify) post their
			// own formats with the same User-Agent.
			return veto("starr-other", agentName(a.prefix))
		}
	}
	for _, p := range otherStarr {
		if strings.HasPrefix(ua, p) {
			return veto("starr-other", agentName(p))
		}
	}

	forgejo := h.Get("X-Forgejo-Event") != ""
	if forgejo || h.Get("X-Gitea-Event") != "" {
		switch {
		case v.isObject("workflow_run") || v.isObject("workflow_job"):
			return Match{Route: "/gitea", Rule: "gitea-actions", Via: ViaHeader}, true, true
		case forgejo && v.isObject("run"):
			return Match{Route: "/forgejo", Rule: "forgejo-actions", Via: ViaHeader}, true, true
		}
		// Push, issue and pull request events: the relay only follows
		// Actions runs.
		if forgejo {
			return veto("gitea-other", "forgejo")
		}
		return veto("gitea-other", "gitea")
	}

	// Gitea sends X-GitHub-Event and X-Gogs-Event too, so these are vetoes
	// only once the Gitea headers are known to be absent.
	switch {
	case h.Get("X-GitHub-Event") != "":
		return veto("github", "github")
	case h.Get("X-Gogs-Event") != "":
		return veto("gogs", "gogs")
	case h.Get("X-Gitlab-Event") != "":
		return veto("gitlab", "gitlab")
	case h.Get("X-Event-Key") != "":
		return veto("bitbucket", "bitbucket")
	case h.Get("Sentry-Hook-Resource") != "":
		return veto("sentry", "sentry")
	}
	return Match{}, false, false
}

func veto(rule, source string) (Match, bool, bool) {
	return Match{Rule: rule, Via: ViaVeto, Source: source}, false, true
}

// agentName is the app a User-Agent prefix names, as a source: "Lidarr/"
// gives "lidarr".
func agentName(prefix string) string {
	return strings.ToLower(strings.TrimSuffix(prefix, "/"))
}

// bodyRule is a provider's signature on the top-level object. Each is written
// against the provider's input struct and its relay/testdata fixtures, and no
// fixture may match two of them.
type bodyRule struct {
	name, route string
	match       func(v *view) bool
}

var (
	backrestEvent = regexp.MustCompile(`^CONDITION_[A-Z_]+$`)
	overseerrType = regexp.MustCompile(`^(MEDIA_\w+|ISSUE_\w+|TEST_NOTIFICATION)$`)
)

var bodyRules = []bodyRule{
	{"grafana", "/grafana", func(v *view) bool {
		// Grafana's webhook is Alertmanager's with version "1" and an
		// orgId; Alertmanager's own sends version "4".
		version, _ := v.str("version")
		return v.isArray("alerts") && v.isString("groupKey") && version != "4" &&
			(version == "1" || v.isNumber("orgId"))
	}},
	{"argocd", "/argocd", func(v *view) bool {
		app, _ := v.str("app")
		return app != "" && v.oneOf("event", "sync-running", "sync-succeeded", "deployed", "sync-failed", "health-degraded")
	}},
	{"backrest", "/backrest", func(v *view) bool {
		event, ok := v.str("event")
		return ok && backrestEvent.MatchString(event)
	}},
	{"changedetection", "/changedetection", func(v *view) bool {
		return v.isString("diff_url") && (v.isString("url") || v.isString("preview_url"))
	}},
	{"gatus", "/gatus", func(v *view) bool {
		return v.isString("endpoint_name") && v.oneOf("status", "TRIGGERED", "RESOLVED")
	}},
	{"jellyfin", "/jellyfin", func(v *view) bool {
		return v.isString("NotificationType") &&
			(v.isString("ServerId") || v.isString("ServerName") || v.isString("ServerVersion"))
	}},
	{"komodo", "/komodo", func(v *view) bool {
		if !v.isNumber("ts") || !v.isBool("resolved") || !v.oneOf("level", "OK", "WARNING", "CRITICAL") || !v.isObject("target") {
			return false
		}
		data := v.object("data")
		return data != nil && data.isString("type")
	}},
	{"overseerr", "/overseerr", func(v *view) bool {
		t, ok := v.str("notification_type")
		return ok && overseerrType.MatchString(t) && v.isString("subject")
	}},
	{"paperless", "/paperless", func(v *view) bool {
		event, _ := v.str("event")
		switch event {
		case "added", "updated":
			return v.isNumber("doc_id")
		case "consumption_started":
			return v.isString("filename")
		}
		return false
	}},
	{"proxmox", "/proxmox", func(v *view) bool {
		for _, k := range []string{"title", "message", "hostname"} {
			if !v.isString(k) {
				return false
			}
		}
		// The handler also takes "test" and "system" as self-test aliases,
		// but Proxmox never sends them; its Test button sends "".
		return v.oneOf("severity", "info", "notice", "warning", "error", "unknown") &&
			v.oneOf("type", "vzdump", "replication", "fencing", "package-updates", "system-mail", "")
	}},
	{"uptimekuma", "/uptimekuma", func(v *view) bool {
		if !v.isString("msg") {
			return false
		}
		// The test notification sends both as null.
		if v.isNull("heartbeat") && v.isNull("monitor") {
			return true
		}
		hb := v.object("heartbeat")
		return hb != nil && v.isObject("monitor") && hb.isNumber("status")
	}},
	{"bazarr", "/bazarr", func(v *view) bool { return apprise(v, "Bazarr") }},
	{"unmanic", "/unmanic", func(v *view) bool { return apprise(v, "Unmanic") }},
}

// apprise matches the body of Apprise's json:// plugin, sent by an app that
// puts its own name at the start of the title.
func apprise(v *view, app string) bool {
	for _, k := range []string{"version", "message", "type"} {
		if !v.isString(k) {
			return false
		}
	}
	title, ok := v.str("title")
	return ok && strings.HasPrefix(title, app)
}

// view is a JSON object's top level, indexed on first use. Values stay raw
// slices of the body until a rule asks for one; nothing is copied.
type view struct {
	body    []byte
	indexed bool
	fields  map[string]json.RawMessage // nil unless the body is an object
}

func (v *view) get(key string) (json.RawMessage, bool) {
	if !v.indexed {
		v.indexed = true
		v.fields = members(v.body)
	}
	raw, ok := v.fields[key]
	return raw, ok
}

// kind is the first byte of a raw JSON value, which names its type.
func (v *view) kind(key string) byte {
	raw, ok := v.get(key)
	if !ok || len(raw) == 0 {
		return 0
	}
	return raw[0]
}

func (v *view) isString(key string) bool { return v.kind(key) == '"' }
func (v *view) isObject(key string) bool { return v.kind(key) == '{' }
func (v *view) isArray(key string) bool  { return v.kind(key) == '[' }
func (v *view) isNull(key string) bool   { return v.kind(key) == 'n' }

func (v *view) isBool(key string) bool {
	k := v.kind(key)
	return k == 't' || k == 'f'
}

func (v *view) isNumber(key string) bool {
	k := v.kind(key)
	return k == '-' || (k >= '0' && k <= '9')
}

// str returns the string at key; ok is false when there is none.
func (v *view) str(key string) (string, bool) {
	raw, ok := v.get(key)
	if !ok || len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	return unquote(raw)
}

// oneOf reports whether key holds a string equal to one of want.
func (v *view) oneOf(key string, want ...string) bool {
	s, ok := v.str(key)
	if !ok {
		return false
	}
	for _, w := range want {
		if s == w {
			return true
		}
	}
	return false
}

// object returns a view of the object at key, or nil.
func (v *view) object(key string) *view {
	raw, ok := v.get(key)
	if !ok || len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	return &view{body: raw}
}

// members indexes the members of the JSON object in body, each value a slice
// of body, the last one winning for a repeated key as with encoding/json. It
// returns nil when body is not a valid JSON object.
func members(body []byte) map[string]json.RawMessage {
	// Valid checks the whole document, so the walk below can trust its
	// structure.
	if !json.Valid(body) {
		return nil
	}
	i := skipSpace(body, 0)
	if body[i] != '{' {
		return nil
	}
	fields := map[string]json.RawMessage{}
	i = skipSpace(body, i+1)
	for body[i] != '}' {
		end := skipString(body, i)
		key, ok := unquote(body[i:end])
		i = skipSpace(body, skipSpace(body, end)+1) // past the colon
		vend := skipValue(body, i)
		if ok {
			fields[key] = body[i:vend:vend]
		}
		i = skipSpace(body, vend)
		if body[i] == ',' {
			i = skipSpace(body, i+1)
		}
	}
	return fields
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// skipString returns the index just past the string starting at b[i].
func skipString(b []byte, i int) int {
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return i
}

// skipValue returns the index just past the value starting at b[i].
func skipValue(b []byte, i int) int {
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for ; i < len(b); i++ {
			switch b[i] {
			case '"':
				i = skipString(b, i) - 1
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
		}
		return i
	}
	for i < len(b) && b[i] != ',' && b[i] != '}' && b[i] != ']' && b[i] != ' ' && b[i] != '\t' && b[i] != '\n' && b[i] != '\r' {
		i++
	}
	return i
}

// unquote decodes a JSON string literal. Plain ASCII without escapes, the
// usual case, is sliced; anything else goes through encoding/json, so the
// result matches what it would decode, invalid UTF-8 included.
func unquote(raw []byte) (string, bool) {
	if len(raw) < 2 {
		return "", false
	}
	plain := true
	for _, c := range raw[1 : len(raw)-1] {
		if c == '\\' || c >= 0x80 || c < 0x20 {
			plain = false
			break
		}
	}
	if plain {
		return string(raw[1 : len(raw)-1]), true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
