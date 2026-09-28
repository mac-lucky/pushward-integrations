package universalhook

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

//go:embed templates/*.html templates/editor.css
var assets embed.FS

var (
	pages     = template.Must(template.ParseFS(assets, "templates/*.html"))
	editorCSS = func() template.CSS {
		b, err := assets.ReadFile("templates/editor.css")
		if err != nil {
			panic(err)
		}
		return template.CSS(b) // #nosec G203 -- our own embedded stylesheet
	}()
	// contentSecurityPolicy allows the one embedded stylesheet, by hash, and
	// nothing else: no script, no image, no request but the form post.
	contentSecurityPolicy = "default-src 'none'; style-src 'sha256-" + styleHash(string(editorCSS)) +
		"'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
)

func styleHash(css string) string {
	sum := sha256.Sum256([]byte(css))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Option caps. The fields table at the bottom of the page shows every path
// and sample whole; a role's options show the end of the path, the part that
// names the field, and a shorter sample, which keeps a page for the largest
// shape the store keeps near 100 KB.
const (
	optionPathRunes   = 64
	optionSampleRunes = 40
	maxOtherOptions   = 64
	// blankRows is how many empty rows each value table offers for new values.
	blankRows = 3
)

// keepValue is the option for a mapped path the stored shape does not have,
// which the editor cannot offer by index. Saving it keeps the path, and
// Validate then says what is wrong with it.
const keepValue = "keep"

// page is what every template's head needs.
type page struct {
	Title string
	CSS   template.CSS
}

func newPage(title string) page {
	return page{Title: title, CSS: editorCSS}
}

type choice struct {
	Value, Label string
	Selected     bool
}

type roleView struct {
	Role, Label, Hint string
	None              bool
	Suggested, Other  []choice
	Scales            []choice
}

type tableView struct {
	Name, Legend, Hint string
	Rows               []rowView
}

type rowView struct {
	Key     string
	Fixed   bool
	Choices []choice
}

type fieldView struct {
	Path, Class, Sample string
}

type editorPage struct {
	page
	Source      string
	Status      string
	StatusLabel string
	StatusNote  string
	Saved       bool
	Errors      []string
	// Action is the form's target, the token relative to the page, so the
	// form posts back to the page it came from behind any path prefix.
	Action              string
	Rev                 int
	Created             int64
	Kinds               []choice
	Roles               []roleView
	Severity, Lifecycle tableView
	Fields              []fieldView
	HasSamples          bool
}

type messageLink struct {
	Href, Label string
}

type messagePage struct {
	page
	Heading, Text string
	Link          *messageLink
}

type listRow struct {
	Href, Source, Kind, Title, Used string
}

type listGroup struct {
	Label string
	Rows  []listRow
}

type listPage struct {
	page
	Groups []listGroup
}

var statusText = map[state.MappingStatus][2]string{
	state.MappingPending: {
		"Waiting for review",
		"Webhooks of this shape already use this mapping. Save to confirm it, changed or not.",
	},
	state.MappingConfirmed: {"Confirmed", "Webhooks of this shape use this mapping."},
	state.MappingRejected: {
		"Sent raw",
		"Webhooks of this shape arrive as a plain list of their fields. Save to map them again.",
	},
}

var kindNames = map[universal.Kind]string{
	universal.KindNotification: "Notification",
	universal.KindAlert:        "Alert card",
	universal.KindProgress:     "Progress card",
}

var kinds = []universal.Kind{universal.KindNotification, universal.KindAlert, universal.KindProgress}

var roleHints = map[universal.Role]string{
	universal.RoleCorrelation: "Events with the same value update one card.",
	universal.RoleSeverity:    "Sets the card's color and how loudly it notifies.",
	universal.RoleLifecycle:   "Tells an ongoing event from one that ended it.",
}

var scaleChoices = []choice{
	{Value: universal.ScaleRatio, Label: "A ratio, 0 to 1"},
	{Value: universal.ScalePercent, Label: "A percentage, 0 to 100"},
}

var (
	severityChoices = []choice{
		{Value: "", Label: "default"},
		{Value: universal.SeverityCritical, Label: "critical"},
		{Value: universal.SeverityWarning, Label: "warning"},
		{Value: universal.SeverityInfo, Label: "info"},
	}
	lifecycleChoices = []choice{
		{Value: "", Label: "default"},
		{Value: universal.LifecycleOngoing, Label: "ongoing"},
		{Value: universal.LifecycleEnded, Label: "ended"},
	}
)

// loaded is a stored row read for the editor.
type loaded struct {
	row     *state.MappingRow
	mapping universal.Mapping
	shape   universal.Shape
	// index maps a path to its position in shape.Fields; the form names
	// fields by position, which stays put for the life of the row.
	index   map[string]int
	samples map[string]string
	cands   map[universal.Role][]string
}

// load decodes a row. Samples older than state.SamplesTTL are left out even
// if the sweep has not dropped them yet.
func load(row *state.MappingRow, now time.Time) (*loaded, error) {
	l := &loaded{row: row, index: map[string]int{}}
	if err := json.Unmarshal(row.Mapping, &l.mapping); err != nil {
		return nil, fmt.Errorf("mapping: %w", err)
	}
	if err := json.Unmarshal(row.Shape, &l.shape); err != nil {
		return nil, fmt.Errorf("shape: %w", err)
	}
	if l.shape.V != universal.ShapeVersion {
		return nil, fmt.Errorf("shape version %d, want %d", l.shape.V, universal.ShapeVersion)
	}
	for i := len(l.shape.Fields) - 1; i >= 0; i-- {
		l.index[l.shape.Fields[i].Path] = i
	}
	if len(row.Samples) > 0 && now.Sub(row.CreatedAt) < state.SamplesTTL {
		_ = json.Unmarshal(row.Samples, &l.samples)
	}
	if len(row.Candidates) > 0 {
		_ = json.Unmarshal(row.Candidates, &l.cands)
	}
	return l, nil
}

// tableRow is one value table row as the form holds it: a key and what it
// means, "" to leave the key to the default.
type tableRow struct {
	Key, Value string
	Fixed      bool
}

// draft is what the editor form shows: the stored mapping, or what a failed
// save submitted.
type draft struct {
	kind   universal.Kind
	paths  map[universal.Role]string
	scale  string
	sv, lv []tableRow
}

func (l *loaded) draft() draft {
	d := draft{kind: l.mapping.Kind, paths: map[universal.Role]string{}, scale: l.mapping.ProgressScale}
	maps.Copy(d.paths, l.mapping.Paths)
	d.sv = storedRows(l.mapping.SeverityValues)
	d.lv = storedRows(l.mapping.LifecycleValues)
	return d
}

func storedRows(t map[string]string) []tableRow {
	rows := make([]tableRow, 0, len(t)+blankRows)
	for _, k := range slices.Sorted(maps.Keys(t)) {
		rows = append(rows, tableRow{Key: k, Value: t[k], Fixed: true})
	}
	for range blankRows {
		rows = append(rows, tableRow{})
	}
	return rows
}

// editorPage renders d for the row in l. at is the version the form carries:
// the row's own, or the one a failed save was made from.
func (l *loaded) editorPage(tok string, d draft, at state.RowVersion, errs []string) editorPage {
	st := statusText[l.row.Status]
	p := editorPage{
		page:        newPage("Webhook mapping"),
		Source:      l.row.Source,
		Status:      string(l.row.Status),
		StatusLabel: st[0],
		StatusNote:  st[1],
		Errors:      errs,
		Action:      tok,
		Rev:         at.Rev,
		Created:     at.CreatedAt.UnixMicro(),
		HasSamples:  len(l.samples) > 0,
	}
	for _, k := range kinds {
		p.Kinds = append(p.Kinds, choice{Value: string(k), Label: kindNames[k], Selected: k == d.kind})
	}
	for _, role := range universal.Roles {
		rv := l.roleView(role, d.paths[role])
		if role == universal.RoleProgress {
			rv.Scales = pick(scaleChoices, d.scale)
		}
		p.Roles = append(p.Roles, rv)
	}
	p.Severity = tableView{
		Name: "sv", Legend: "Severity values",
		Hint: "What each value of the severity field means. Values not listed are read as the relay would guess them.",
		Rows: rowViews(d.sv, severityChoices),
	}
	p.Lifecycle = tableView{
		Name: "lv", Legend: "Status values",
		Hint: "Which values of the status field end a card. Values not listed are read as the relay would guess them.",
		Rows: rowViews(d.lv, lifecycleChoices),
	}
	for i := range l.shape.Fields {
		f := &l.shape.Fields[i]
		p.Fields = append(p.Fields, fieldView{Path: f.Path, Class: string(f.Class), Sample: l.samples[f.Path]})
	}
	return p
}

// pick copies choices with value v selected; with no match the first one is,
// as a browser would show it.
func pick(choices []choice, v string) []choice {
	out := slices.Clone(choices)
	for i := range out {
		out[i].Selected = out[i].Value == v
	}
	return out
}

func rowViews(rows []tableRow, choices []choice) []rowView {
	out := make([]rowView, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowView{Key: r.Key, Fixed: r.Fixed, Choices: pick(choices, r.Value)})
	}
	return out
}

// roleView offers the stored candidates for a role first, then every other
// field the role accepts, up to maxOtherOptions. The path mapped now is
// always among them.
func (l *loaded) roleView(role universal.Role, current string) roleView {
	rv := roleView{Role: string(role), Label: roleLabels[role], Hint: roleHints[role], None: current == ""}
	seen := map[int]bool{}
	for _, p := range l.cands[role] {
		i, ok := l.index[p]
		if !ok || seen[i] || !universal.Mappable(role, &l.shape.Fields[i]) {
			continue
		}
		seen[i] = true
		rv.Suggested = append(rv.Suggested, l.option(i, current))
	}
	for i := range l.shape.Fields {
		if len(rv.Other) == maxOtherOptions {
			break
		}
		if !seen[i] && universal.Mappable(role, &l.shape.Fields[i]) {
			seen[i] = true
			rv.Other = append(rv.Other, l.option(i, current))
		}
	}
	if current == "" {
		return rv
	}
	switch i, ok := l.index[current]; {
	case !ok:
		rv.Other = append(rv.Other, choice{Value: keepValue, Label: tailRunes(current, optionPathRunes) + " (not in this payload)", Selected: true})
	case !seen[i]:
		rv.Other = append(rv.Other, l.option(i, current))
	}
	return rv
}

func (l *loaded) option(i int, current string) choice {
	f := &l.shape.Fields[i]
	label := tailRunes(f.Path, optionPathRunes)
	if s := l.samples[f.Path]; s != "" {
		label += ` = "` + text.Truncate(s, optionSampleRunes) + `"`
	}
	return choice{Value: strconv.Itoa(i), Label: label, Selected: f.Path == current}
}

// errBadForm is a form the editor page cannot have sent: a missing version,
// a field index past the shape, value tables out of step.
var errBadForm = errors.New("universalhook: malformed editor form")

// Editor operations, as universal_editor_requests_total labels them.
const (
	opView = "view"
	opSave = "save"
	opRaw  = "raw"
	opList = "list"
)

// readVersion reads which button was pressed and the row version the form
// was made from.
func readVersion(f url.Values) (string, state.RowVersion, error) {
	var op string
	switch f.Get("op") {
	case "", "save":
		op = opSave
	case "raw":
		op = opRaw
	default:
		return "", state.RowVersion{}, errBadForm
	}
	// rev is an int4 column: a value past it would fail in the store.
	rev, err := strconv.ParseInt(f.Get("rev"), 10, 32)
	if err != nil || rev < 0 {
		return op, state.RowVersion{}, errBadForm
	}
	created, err := strconv.ParseInt(f.Get("created"), 10, 64)
	if err != nil {
		return op, state.RowVersion{}, errBadForm
	}
	return op, state.RowVersion{Rev: int(rev), CreatedAt: time.UnixMicro(created)}, nil
}

// readDraft reads the mapping fields of a form made for the row in l.
func (l *loaded) readDraft(f url.Values) (draft, error) {
	d := draft{kind: universal.Kind(f.Get("kind")), paths: map[universal.Role]string{}, scale: f.Get("ps")}
	for _, role := range universal.Roles {
		switch v := f.Get("p." + string(role)); v {
		case "":
		case keepValue:
			if p := l.mapping.Paths[role]; p != "" {
				d.paths[role] = p
			}
		default:
			i, err := strconv.Atoi(v)
			if err != nil || i < 0 || i >= len(l.shape.Fields) {
				return d, errBadForm
			}
			d.paths[role] = l.shape.Fields[i].Path
		}
	}
	var err error
	if d.sv, err = readRows(f, "sv"); err != nil {
		return d, err
	}
	d.lv, err = readRows(f, "lv")
	return d, err
}

// readRows reads a value table. The page lists the stored keys first, as
// hidden name.k inputs, then the blank rows as name.n text inputs; the
// name.v selects follow the same order. No page has more than
// MaxTableEntries+blankRows rows, and a form with more is refused rather than
// rendered back at many times its size.
func readRows(f url.Values, name string) ([]tableRow, error) {
	fixed, typed, values := f[name+".k"], f[name+".n"], f[name+".v"]
	if len(fixed)+len(typed) != len(values) || len(values) > universal.MaxTableEntries+blankRows {
		return nil, errBadForm
	}
	rows := make([]tableRow, 0, len(values))
	for i, v := range values {
		r := tableRow{Value: v, Fixed: i < len(fixed)}
		if r.Fixed {
			r.Key = fixed[i]
		} else {
			r.Key = typed[i-len(fixed)]
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// mapping builds the mapping d describes and checks it against the shape.
// It returns every problem found: Validate's first, in role order, then the
// value tables'.
func (d draft) mapping(l *loaded) (universal.Mapping, []string) {
	m := universal.Mapping{V: universal.MappingVersion, Kind: d.kind, Paths: map[universal.Role]string{}}
	classes := map[universal.Role]universal.ValueClass{}
	for _, role := range universal.Roles {
		p := d.paths[role]
		if p == "" {
			continue
		}
		m.Paths[role] = p
		if i, ok := l.index[p]; ok {
			classes[role] = l.shape.Fields[i].Class
		}
	}
	if len(classes) > 0 {
		m.Classes = classes
	}
	if m.Paths[universal.RoleProgress] != "" {
		m.ProgressScale = d.scale
	}
	var tableErrs []string
	// A table only means something with its field mapped; without one it
	// is dropped rather than kept unused.
	if m.Paths[universal.RoleSeverity] != "" {
		m.SeverityValues, tableErrs = valueTable("Severity values", d.sv, tableErrs)
	}
	if m.Paths[universal.RoleLifecycle] != "" {
		m.LifecycleValues, tableErrs = valueTable("Status values", d.lv, tableErrs)
	}
	var errs []string
	if err := m.Validate(l.shape); err != nil {
		errs = splitErrors(err)
	}
	return m, capErrors(append(errs, tableErrs...))
}

// maxErrorLines caps the problems a failed save lists.
const maxErrorLines = 20

func capErrors(errs []string) []string {
	if len(errs) <= maxErrorLines {
		return errs
	}
	more := len(errs) - maxErrorLines + 1
	return append(errs[:maxErrorLines-1:maxErrorLines-1], fmt.Sprintf("And %d more.", more))
}

// valueTable turns table rows into a mapping table. Keys are stored in
// universal.NormValue form; a blank key or a row left at "default" is
// skipped. A key that is too long or looks secret (the table is stored and
// exported with the mapping, samples never are) is refused, and so is one
// listed twice with different meanings. Validate checks the rest.
func valueTable(label string, rows []tableRow, errs []string) (map[string]string, []string) {
	out := map[string]string{}
	for _, r := range rows {
		k := universal.NormValue(r.Key)
		if k == "" || r.Value == "" {
			continue
		}
		switch prev, dup := out[k]; {
		case utf8.RuneCountInString(k) > universal.MaxTableKeyRunes:
			errs = append(errs, fmt.Sprintf("%s: %q is longer than %d characters.", label, tailRunes(k, 24), universal.MaxTableKeyRunes))
		case secretLike(k):
			errs = append(errs, fmt.Sprintf("%s: a value that looks like a secret or personal data cannot be stored.", label))
		case dup && prev != r.Value:
			errs = append(errs, fmt.Sprintf("%s: %q is listed twice.", label, k))
		default:
			out[k] = r.Value
		}
	}
	if len(out) == 0 {
		return nil, errs
	}
	return out, errs
}

// secretLike reports a table key that universal.Display would not show as
// it is: a credential, an email address, a generated token, a long hex
// string, a card or phone number.
func secretLike(k string) bool {
	f := universal.Field{Path: "state", Value: k, Type: universal.TypeString}
	switch universal.ClassOf(f.Path, f) {
	case universal.ClassSecret, universal.ClassEmail:
		return true
	}
	return universal.Display(f.Path, f, 4*universal.MaxTableKeyRunes) != k
}

// splitErrors lists the errors errors.Join put together, as sentences.
func splitErrors(err error) []string {
	errs := []error{err}
	var j interface{ Unwrap() []error }
	if errors.As(err, &j) {
		errs = j.Unwrap()
	}
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, text.Capitalize(e.Error())+".")
	}
	return out
}
