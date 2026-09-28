package universal

import (
	"cmp"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Role is one question a mapping answers about a payload: which field plays
// this part.
type Role string

const (
	RoleTitle       Role = "title"
	RoleBody        Role = "body"
	RoleURL         Role = "url"
	RoleCorrelation Role = "correlation"
	RoleProgress    Role = "progress"
	RoleSeverity    Role = "severity"
	RoleLifecycle   Role = "lifecycle"
)

// Roles lists every role in the order Propose fills them.
var Roles = []Role{RoleTitle, RoleBody, RoleURL, RoleCorrelation, RoleProgress, RoleSeverity, RoleLifecycle}

// Kind is what a mapped event becomes in PushWard.
type Kind string

const (
	KindNotification Kind = "notification"
	KindAlert        Kind = "alert"
	KindProgress     Kind = "progress"
)

// PushWard-side values for the value tables.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"

	LifecycleOngoing = "ongoing"
	LifecycleEnded   = "ended"
)

// Proposal is a suggested mapping for one payload shape. Every role holds a
// path from the flattened payload, or "" for none.
type Proposal struct {
	Title       string `json:"title"`
	Body        string `json:"body"`
	URL         string `json:"url"`
	Correlation string `json:"correlation"`
	Progress    string `json:"progress"`
	Severity    string `json:"severity"`
	Lifecycle   string `json:"lifecycle"`
	Kind        Kind   `json:"kind"`

	// SeverityValues and LifecycleValues map the values seen in the sample to
	// PushWard's severity (critical, warning, info) and lifecycle (ongoing,
	// ended). Values the heuristic cannot place are left out for the user to
	// fill in.
	SeverityValues  map[string]string `json:"severity_values,omitempty"`
	LifecycleValues map[string]string `json:"lifecycle_values,omitempty"`
}

// Path returns the proposed path for a role.
func (p *Proposal) Path(r Role) string {
	switch r {
	case RoleTitle:
		return p.Title
	case RoleBody:
		return p.Body
	case RoleURL:
		return p.URL
	case RoleCorrelation:
		return p.Correlation
	case RoleProgress:
		return p.Progress
	case RoleSeverity:
		return p.Severity
	case RoleLifecycle:
		return p.Lifecycle
	}
	return ""
}

func (p *Proposal) set(r Role, path string) {
	switch r {
	case RoleTitle:
		p.Title = path
	case RoleBody:
		p.Body = path
	case RoleURL:
		p.URL = path
	case RoleCorrelation:
		p.Correlation = path
	case RoleProgress:
		p.Progress = path
	case RoleSeverity:
		p.Severity = path
	case RoleLifecycle:
		p.Lifecycle = path
	}
}

// Candidate is a field scored for one role.
type Candidate struct {
	Path  string  `json:"path"`
	Score float64 `json:"score"`
}

// minScore is the score a role's best candidate needs before Propose uses it.
// Title is low on purpose: a notification with a weak title beats one with
// none.
var minScore = map[Role]float64{
	RoleTitle:       0.5,
	RoleBody:        1.5,
	RoleURL:         1.5,
	RoleCorrelation: 2,
	RoleProgress:    2.5,
	RoleSeverity:    3,
	RoleLifecycle:   3,
}

// Propose picks a field for every role and a kind. It is deterministic and
// cheap enough to run on every unmapped event.
func Propose(fields []Field) Proposal {
	return ProposeShapes(ShapesOf(fields))
}

// ProposeShapes is Propose over shapes, so a stored shape gets the same
// proposal its payload did. The value tables are keyed by vocabulary words
// (normValue form), never by sample text.
func ProposeShapes(shapes []ShapeField) Proposal {
	views := viewsOf(shapes)
	var p Proposal
	used := map[string]bool{}
	for _, r := range Roles {
		for _, c := range rank(views, r) {
			if c.Score < minScore[r] {
				break
			}
			// Title and body are both text; one field cannot be both, and
			// neither can double as the lifecycle or severity enum.
			if used[c.Path] && r != RoleCorrelation && r != RoleURL {
				continue
			}
			p.set(r, c.Path)
			used[c.Path] = true
			break
		}
	}
	byPath := make(map[string]*ShapeField, len(shapes))
	for i := range shapes {
		byPath[shapes[i].Path] = &shapes[i]
	}
	if f := byPath[p.Severity]; f != nil && p.Severity != "" {
		if v := severityValues[f.Vocab]; v != "" {
			p.SeverityValues = map[string]string{f.Vocab: v}
		}
	}
	if f := byPath[p.Lifecycle]; f != nil && p.Lifecycle != "" {
		if w, v := lifecycleWord(f); v != "" {
			p.LifecycleValues = map[string]string{w: v}
		}
	}
	p.Kind = kindOf(views, &p, byPath)
	return p
}

// Rank scores every field for a role, best first. Fields that cannot play the
// role at all (a boolean title, a non-URL link, a secret or an email for
// anything) are left out. Ties keep document order.
func Rank(fields []Field, r Role) []Candidate {
	return RankShapes(ShapesOf(fields), r)
}

// RankShapes is Rank over shapes.
func RankShapes(shapes []ShapeField, r Role) []Candidate {
	return rank(viewsOf(shapes), r)
}

func rank(views []view, r Role) []Candidate {
	out := make([]Candidate, 0, len(views))
	for i := range views {
		s := score(r, &views[i], i)
		if math.IsInf(s, -1) {
			continue
		}
		out = append(out, Candidate{Path: views[i].Path, Score: math.Round(s*1000) / 1000})
	}
	slices.SortStableFunc(out, func(a, b Candidate) int { return cmp.Compare(b.Score, a.Score) })
	return out
}

// Key-name vocabularies. Weights are summed over the tokens of a field's own
// key; parent segments count through the context lists further down.
var (
	titleWords = map[string]float64{
		"title": 3, "name": 2, "summary": 2.5, "subject": 2.5, "headline": 2.5,
		"alertname": 3, "message": 1.2, "text": 1, "label": 0.5, "topic": 1,
		"display": 0.5, "check": 0.5, "monitor": 0.5, "rule": 0.5, "caption": 2,
	}
	bodyWords = map[string]float64{
		"description": 3, "message": 2.5, "body": 3, "text": 2, "details": 2,
		"detail": 2, "content": 2, "summary": 1.2, "output": 1.5, "reason": 1.5,
		"error": 1.5, "msg": 2.5, "info": 0.5, "note": 1, "notes": 1, "comment": 1,
		"log": 1, "status": 0.3, "cause": 1.5, "explanation": 2,
	}
	urlWords = map[string]float64{
		"url": 3, "link": 2.5, "href": 2.5, "uri": 1.5, "permalink": 3,
		"html": 1, "web": 1, "dashboard": 1, "generator": 0.5, "details": 0.5,
		"view": 0.5, "external": 0.5, "page": 0.5, "incident": 0.3, "alert": 0.3,
		"avatar": -4, "icon": -4, "image": -3, "logo": -4, "thumb": -4, "thumbnail": -4,
		"api": -1.5, "silence": -1, "callback": -2, "git": -1, "ssh": -3, "clone": -2,
		"events": -0.5, "hooks": -2, "self": -1,
	}
	corrWords = map[string]float64{
		"id": 2, "uuid": 2.5, "guid": 2, "key": 1, "fingerprint": 3, "groupkey": 3,
		"dedup": 3, "deduplication": 3, "correlation": 3, "alias": 1.5, "number": 0.5,
		"run": 0.8, "incident": 1, "alert": 0.5, "check": 0.5, "monitor": 0.5,
		"job": 0.5, "pipeline": 0.8, "build": 0.5, "deployment": 0.5, "issue": 0.5,
		"task": 0.5, "session": 0.5, "group": 0.5, "slug": 1, "ref": 0.3,
	}
	progressWords = map[string]float64{
		"progress": 3, "percent": 3, "percentage": 3, "pct": 2.5, "completion": 2,
		"completed": 0.5, "done": 0.5, "ratio": 1, "fraction": 1,
	}
	severityWords = map[string]float64{
		"severity": 3, "level": 2, "priority": 2.5, "urgency": 2.5, "importance": 2,
		"criticality": 3, "sev": 2.5, "impact": 1,
	}
	lifecycleWords = map[string]float64{
		"status": 2.5, "state": 2.5, "event": 1.5, "action": 2, "type": 1,
		"phase": 2, "stage": 1, "result": 1.5, "conclusion": 2, "outcome": 1.5,
		"kind": 0.5, "transition": 1.5, "trigger": 0.5,
	}

	// Parent objects that describe context rather than the subject: a
	// repository name is not a build's title, a sender id would merge every
	// event into one card.
	contextParents = map[string]bool{
		"repository": true, "repo": true, "project": true, "sender": true,
		"user": true, "author": true, "owner": true, "organization": true,
		"org": true, "actor": true, "installation": true, "commit": true,
		"commits": true, "account": true, "team": true, "workspace": true,
		"head": true, "base": true, "creator": true, "assignee": true,
		"assignees": true, "reporter": true, "committer": true, "pusher": true,
		"environment": true, "server": true, "host": true, "customer": true,
		"metadata": true, "meta": true, "headers": true, "links": true,
	}
	// Words marking an id that changes with every delivery.
	perDelivery = map[string]bool{
		"event": true, "delivery": true, "request": true, "message": true,
		"notification": true, "webhook": true, "hook": true, "trace": true,
		"span": true, "nonce": true, "attempt": true, "retry": true,
	}
	// Words marking a key that is not text a person reads.
	machineWords = map[string]bool{
		"id": true, "uuid": true, "guid": true, "url": true, "uri": true,
		"href": true, "link": true, "token": true, "secret": true, "hash": true,
		"sha": true, "email": true, "avatar": true, "icon": true, "color": true,
		"colour": true, "key": true, "signature": true, "timestamp": true,
		"time": true, "date": true, "at": true, "created": true, "updated": true,
		"version": true, "type": true, "kind": true, "count": true, "size": true,
		"fingerprint": true, "slug": true, "path": true, "file": true, "ip": true,
		"port": true, "code": true, "number": true, "duration": true,
	}
	// Numeric keys that are sizes and durations, not completion.
	notProgress = map[string]bool{
		"count": true, "total": true, "size": true, "bytes": true,
		"duration": true, "eta": true, "time": true, "seconds": true,
		"ms": true, "id": true, "number": true, "version": true, "port": true,
	}

	severityValues = map[string]string{
		"critical": SeverityCritical, "crit": SeverityCritical, "fatal": SeverityCritical,
		"emergency": SeverityCritical, "emerg": SeverityCritical, "disaster": SeverityCritical,
		"high": SeverityCritical, "major": SeverityCritical, "error": SeverityCritical,
		"err": SeverityCritical, "alert": SeverityCritical, "urgent": SeverityCritical,
		"p1": SeverityCritical, "p2": SeverityCritical, "sev1": SeverityCritical,
		"sev2": SeverityCritical, "page": SeverityCritical, "severe": SeverityCritical,
		"warning": SeverityWarning, "warn": SeverityWarning, "medium": SeverityWarning,
		"average": SeverityWarning, "minor": SeverityWarning, "moderate": SeverityWarning,
		"p3": SeverityWarning, "sev3": SeverityWarning,
		"info": SeverityInfo, "information": SeverityInfo, "informational": SeverityInfo,
		"low": SeverityInfo, "notice": SeverityInfo, "debug": SeverityInfo,
		"p4": SeverityInfo, "p5": SeverityInfo, "sev4": SeverityInfo, "ok": SeverityInfo,
		"none": SeverityInfo, "not_classified": SeverityInfo, "trivial": SeverityInfo,
	}
	lifecycleValues = map[string]string{
		"firing": LifecycleOngoing, "triggered": LifecycleOngoing, "trigger": LifecycleOngoing,
		"alerting": LifecycleOngoing, "problem": LifecycleOngoing, "down": LifecycleOngoing,
		"running": LifecycleOngoing, "pending": LifecycleOngoing, "queued": LifecycleOngoing,
		"started": LifecycleOngoing, "start": LifecycleOngoing, "in_progress": LifecycleOngoing,
		"inprogress": LifecycleOngoing, "created": LifecycleOngoing, "new": LifecycleOngoing,
		"update": LifecycleOngoing, "updated": LifecycleOngoing, "open": LifecycleOngoing,
		"opened": LifecycleOngoing, "acknowledged": LifecycleOngoing, "play": LifecycleOngoing,
		"resume": LifecycleOngoing, "building": LifecycleOngoing, "active": LifecycleOngoing,
		"requested": LifecycleOngoing, "waiting": LifecycleOngoing, "deploying": LifecycleOngoing,
		"reopened": LifecycleOngoing, "paused": LifecycleOngoing, "pause": LifecycleOngoing,
		"degraded": LifecycleOngoing, "investigating": LifecycleOngoing,
		"identified": LifecycleOngoing, "monitoring": LifecycleOngoing, "alarm": LifecycleOngoing,
		"resolved": LifecycleEnded, "ok": LifecycleEnded, "up": LifecycleEnded,
		"recovery": LifecycleEnded, "recovered": LifecycleEnded, "success": LifecycleEnded,
		"succeeded": LifecycleEnded, "successful": LifecycleEnded, "passed": LifecycleEnded,
		"failed": LifecycleEnded, "failure": LifecycleEnded, "completed": LifecycleEnded,
		"complete": LifecycleEnded, "finished": LifecycleEnded, "done": LifecycleEnded,
		"canceled": LifecycleEnded, "cancelled": LifecycleEnded, "skipped": LifecycleEnded,
		"closed": LifecycleEnded, "end": LifecycleEnded, "ended": LifecycleEnded,
		"stop": LifecycleEnded, "stopped": LifecycleEnded, "deleted": LifecycleEnded,
		"expired": LifecycleEnded, "timeout": LifecycleEnded, "timed_out": LifecycleEnded,
		"errored": LifecycleEnded, "error": LifecycleEnded, "aborted": LifecycleEnded,
		"merged": LifecycleEnded, "deployed": LifecycleEnded, "ready": LifecycleEnded,
		"postmortem": LifecycleEnded,
	}
	// Lifecycle values that mark a job with a start and an end rather than a
	// condition that fires.
	progressStates = map[string]bool{
		"running": true, "pending": true, "queued": true, "started": true,
		"start": true, "in_progress": true, "inprogress": true, "building": true,
		"success": true, "succeeded": true, "successful": true, "failed": true,
		"completed": true, "complete": true, "finished": true, "canceled": true,
		"cancelled": true, "skipped": true, "deploying": true, "deployed": true,
		"requested": true, "waiting": true, "play": true, "pause": true, "paused": true,
		"resume": true, "stop": true, "stopped": true, "passed": true, "aborted": true,
		"timed_out": true, "errored": true,
	}
	alertStates = map[string]bool{
		"firing": true, "resolved": true, "triggered": true, "trigger": true,
		"alerting": true, "problem": true, "down": true, "up": true, "ok": true,
		"recovery": true, "recovered": true, "acknowledged": true, "alarm": true,
		"degraded": true, "investigating": true, "identified": true,
		"monitoring": true, "postmortem": true,
	}
	// Path words that point at a job or at an alert. They only break ties.
	progressPathWords = map[string]bool{
		"pipeline": true, "build": true, "job": true, "run": true, "deploy": true,
		"deployment": true, "backup": true, "download": true, "print": true,
		"task": true, "workflow": true, "playback": true, "transcode": true,
		"sync": true, "upload": true, "import": true, "export": true,
	}
	alertPathWords = map[string]bool{
		"alert": true, "alerts": true, "incident": true, "alarm": true,
		"monitor": true, "check": true, "problem": true, "trigger": true,
		"outage": true, "downtime": true,
	}
)

var (
	camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	uuidValue     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexValue      = regexp.MustCompile(`^[0-9a-fA-F]{8,}$`)
	intValue      = regexp.MustCompile(`^-?[0-9]+$`)
	timeValue     = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}([T ][0-9]{2}:[0-9]{2}.*)?$`)
	enumValue     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:/-]{0,39}$`)
	idToken       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{2,127}$`)
)

// tokens splits a key into lowercase words: "html_url" and "htmlUrl" both
// give [html url].
func tokens(key string) []string {
	key = strings.TrimSuffix(key, "[]")
	key = camelBoundary.ReplaceAllString(key, "${1}_${2}")
	return strings.FieldsFunc(strings.ToLower(key), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// segments splits a path into its keys, dropping array markers and "*".
func segments(path string) []string {
	parts := strings.Split(path, ".")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimRight(p, "[]")
		if p != "" && p != "*" {
			out = append(out, p)
		}
	}
	return out
}

// view is a shape with its path split into words, done once per payload
// rather than once per role.
type view struct {
	*ShapeField
	key     []string // tokens of the leaf key
	parents []string // tokens of every parent key
	depth   int
	inArray bool
}

func viewsOf(shapes []ShapeField) []view {
	out := make([]view, len(shapes))
	for i := range shapes {
		f := &shapes[i]
		segs := segments(f.Path)
		v := view{ShapeField: f, depth: len(segs), inArray: strings.Contains(f.Path, "[]")}
		if len(segs) > 0 {
			v.key = tokens(segs[len(segs)-1])
			for _, p := range segs[:len(segs)-1] {
				v.parents = append(v.parents, tokens(p)...)
			}
		}
		out[i] = v
	}
	return out
}

func (v *view) is(f Flags) bool { return v.Flags&f != 0 }

func (v *view) str() bool { return v.Type == TypeString }

func (v *view) isNum() bool { return v.Type == TypeNumber && v.is(FlagNum) }

func (v *view) inRange(ranges ...string) bool { return slices.Contains(ranges, v.Range) }

func wordScore(words map[string]float64, toks []string) float64 {
	total := 0.0
	for _, t := range toks {
		total += words[t]
	}
	// A compound key names the concept once: "alert_name" should not beat
	// "title" just by having two tokens.
	return math.Min(total, 3.5)
}

func hasAny(toks []string, set map[string]bool) bool {
	return slices.ContainsFunc(toks, func(t string) bool { return set[t] })
}

func score(r Role, v *view, index int) float64 {
	if len(v.key) == 0 || !rankable(r, v.ShapeField) {
		return math.Inf(-1)
	}
	// Earlier fields win ties: services put the important keys first more
	// often than not.
	order := -0.002 * float64(index)
	switch r {
	case RoleTitle:
		return scoreTitle(v) + order
	case RoleBody:
		return scoreBody(v) + order
	case RoleURL:
		return scoreURL(v) + order
	case RoleCorrelation:
		return scoreCorrelation(v) + order
	case RoleProgress:
		return scoreProgress(v) + order
	case RoleSeverity:
		return scoreSeverity(v) + order
	case RoleLifecycle:
		return scoreLifecycle(v) + order
	}
	return math.Inf(-1)
}

func scoreTitle(s *view) float64 {
	if !s.str() || s.Runes == 0 || s.is(FlagURL) || !s.is(FlagLetters) {
		return math.Inf(-1)
	}
	v := wordScore(titleWords, s.key)
	if hasAny(s.key, machineWords) && v < 2 {
		v -= 2
	}
	switch {
	case s.Runes <= 2:
		v -= 2
	case s.Runes <= 120:
		v += 0.5
	case s.Runes <= 200:
		v -= 0.5
	default:
		v -= 1.5
	}
	if s.is(FlagID) || s.is(FlagTime) {
		v -= 2
	}
	if hasAny(s.parents, contextParents) {
		v -= 1.2
	}
	if s.Words > 1 {
		v += 0.3
	}
	return v - 0.15*float64(s.depth) - boolf(s.inArray, 0.3)
}

func scoreBody(s *view) float64 {
	if !s.str() || s.Runes == 0 || s.is(FlagURL) || !s.is(FlagLetters) {
		return math.Inf(-1)
	}
	v := wordScore(bodyWords, s.key)
	if hasAny(s.key, machineWords) && v < 2 {
		v -= 2
	}
	switch {
	case s.Words >= 4:
		v += 1
	case s.Words >= 2:
		v += 0.3
	default:
		v -= 1
	}
	if s.is(FlagID) || s.is(FlagTime) || s.is(FlagEnum) {
		v -= 1.5
	}
	if hasAny(s.parents, contextParents) {
		v -= 1.2
	}
	return v - 0.1*float64(s.depth) - boolf(s.inArray, 0.3)
}

func scoreURL(s *view) float64 {
	if !s.is(FlagURL) {
		return math.Inf(-1)
	}
	v := 1 + wordScore(urlWords, s.key)
	for _, t := range s.parents {
		if w := urlWords[t]; w < 0 {
			v += w
		}
	}
	if hasAny(s.parents, contextParents) {
		v -= 1.5
	}
	return v - 0.1*float64(s.depth) - boolf(s.inArray, 0.3)
}

func scoreCorrelation(s *view) float64 {
	if s.Type != TypeString && s.Type != TypeNumber {
		return math.Inf(-1)
	}
	if s.is(FlagURL) || s.is(FlagTime) || s.Words > 1 || s.Runes == 0 || s.Runes > 128 {
		return math.Inf(-1)
	}
	v := wordScore(corrWords, s.key)
	idKey := slices.ContainsFunc(s.key, func(t string) bool {
		switch t {
		case "id", "uuid", "guid", "key", "fingerprint", "groupkey", "dedup", "alias", "slug":
			return true
		}
		return false
	})
	if !idKey {
		v -= 1.5
	}
	if s.is(FlagID) || (s.str() && s.is(FlagIDToken)) {
		v += 0.8
	}
	if hasAny(s.key, perDelivery) || hasAny(s.parents, perDelivery) {
		v -= 2.5
	}
	if hasAny(s.parents, contextParents) {
		v -= 2
	}
	if hasAny(s.key, notProgress) && !idKey {
		v -= 1
	}
	return v - 0.2*float64(s.depth) - boolf(s.inArray, 0.8)
}

func scoreProgress(s *view) float64 {
	if !s.isNum() || !s.inRange(Range0To1, Range1To10, Range10To1h) {
		return math.Inf(-1)
	}
	v := wordScore(progressWords, s.key)
	if hasAny(s.key, notProgress) {
		v -= 2
	}
	return v - 0.05*float64(s.depth)
}

func scoreSeverity(s *view) float64 {
	if s.Type != TypeString && s.Type != TypeNumber {
		return math.Inf(-1)
	}
	if s.Words > 2 || s.is(FlagURL) || s.is(FlagTime) {
		return math.Inf(-1)
	}
	v := wordScore(severityWords, s.key)
	if _, ok := severityValues[s.Vocab]; ok {
		v += 2
	} else if s.isNum() && s.inRange(Range0To1, Range1To10) {
		v += 0.3
	} else if !s.is(FlagEnum) {
		v -= 1
	}
	if hasAny(s.parents, contextParents) {
		v -= 1
	}
	return v - 0.1*float64(s.depth) - boolf(s.inArray, 0.3)
}

func scoreLifecycle(s *view) float64 {
	if s.Type != TypeString {
		return math.Inf(-1)
	}
	if s.Words > 2 || s.is(FlagURL) || s.is(FlagTime) || s.is(FlagID) {
		return math.Inf(-1)
	}
	v := wordScore(lifecycleWords, s.key)
	if _, ok := lifecycleValues[s.Vocab]; ok {
		v += 2.5
	} else if _, state := lifecycleWord(s.ShapeField); state != "" {
		// "media.play", "issue.resolved", "ALERT_TRIGGERED"
		v += 2
	} else if !s.is(FlagEnum) {
		v -= 1.5
	}
	if hasAny(s.parents, contextParents) {
		v -= 1.2
	}
	return v - 0.1*float64(s.depth) - boolf(s.inArray, 0.4)
}

var normReplacer = strings.NewReplacer(" ", "_", "-", "_")

func normValue(v string) string {
	return normReplacer.Replace(strings.ToLower(strings.TrimSpace(v)))
}

// tailSeparators split a value into words for valueTail: the ones normValue
// already turned into underscores, as before, plus the path-like ones.
const tailSeparators = "._:/- "

// valueTail returns a value's last word in normValue form ("alert.resolved",
// "sync-failed", "CONDITION_SNAPSHOT_END", "Sync failed"), or "" when it has
// one word only or one of the two words before it negates it ("Not
// Resolved", "not-ready", "No Error", "not yet resolved", "never completed"):
// a negated state must not end a card.
func valueTail(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	i := strings.LastIndexAny(v, tailSeparators)
	if i < 0 {
		return ""
	}
	head := v[:i]
	for range 2 {
		j := strings.LastIndexAny(head, tailSeparators)
		switch head[j+1:] {
		case "not", "no", "non", "un", "never", "yet":
			return ""
		}
		if j < 0 {
			break
		}
		head = head[:j]
	}
	return normValue(v[i+1:])
}

func boolf(b bool, w float64) float64 {
	if b {
		return w
	}
	return 0
}

func severityOf(v string) string {
	return severityValues[normValue(v)]
}

func lifecycleOf(v string) string {
	n := normValue(v)
	if s, ok := lifecycleValues[n]; ok {
		return s
	}
	return lifecycleValues[valueTail(v)]
}

// lifecycleWord is lifecycleOf for a shape. It also returns the word it
// matched, which keys the proposal's value table.
func lifecycleWord(f *ShapeField) (word, state string) {
	if s, ok := lifecycleValues[f.Vocab]; ok {
		return f.Vocab, s
	}
	if s := lifecycleValues[valueTail(f.Vocab)]; s != "" {
		return f.Vocab, s
	}
	if s := lifecycleValues[f.LastWord]; s != "" {
		return f.LastWord, s
	}
	return "", ""
}

// kindOf votes between alert and progress; a payload that makes a weak case
// for both is a notification.
func kindOf(views []view, p *Proposal, byPath map[string]*ShapeField) Kind {
	var alert, progress float64
	if p.Progress != "" {
		progress += 3
	}
	if p.Severity != "" {
		alert += 2
	}
	if f := byPath[p.Lifecycle]; f != nil && p.Lifecycle != "" {
		n := f.Vocab
		if n == "" {
			n = f.LastWord
		} else if !progressStates[n] && !alertStates[n] {
			n = valueTail(n)
		}
		if progressStates[n] {
			progress += 2
		}
		if alertStates[n] {
			alert += 2.5
		}
	}
	// Values name the subject as often as keys do: "object_kind":
	// "pipeline", "type": "alert".
	var progressWord, alertWord bool
	for i := range views {
		v := &views[i]
		progressWord = progressWord || progressPathWords[v.Vocab] || hasAny(v.key, progressPathWords) || hasAny(v.parents, progressPathWords)
		alertWord = alertWord || alertPathWords[v.Vocab] || hasAny(v.key, alertPathWords) || hasAny(v.parents, alertPathWords)
	}
	if progressWord {
		progress++
	}
	if alertWord {
		alert++
	}
	switch {
	case alert < 2 && progress < 2:
		return KindNotification
	case alert >= progress:
		return KindAlert
	default:
		return KindProgress
	}
}
