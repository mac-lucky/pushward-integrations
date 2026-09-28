package universal

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// MappingVersion is the version of the Mapping JSON layout.
const MappingVersion = 1

// Progress scales: how a progress sample reads as a fraction.
const (
	ScaleRatio   = "ratio"   // 0..1
	ScalePercent = "percent" // 0..100
)

// Mapping and shape caps.
const (
	MaxShapeBytes    = 16 << 10
	MaxTableEntries  = 16
	MaxTableKeyRunes = 64
)

// Mapping is a decided mapping for one payload shape: which path plays each
// role and how the values of the severity and lifecycle fields translate.
// It holds paths and vocabulary words only, no sample text.
type Mapping struct {
	V     int             `json:"v"`
	Kind  Kind            `json:"k"`
	Paths map[Role]string `json:"p"`
	// Classes records the class each mapped field had in the sample the
	// mapping was made from.
	Classes         map[Role]ValueClass `json:"c,omitempty"`
	ProgressScale   string              `json:"ps,omitempty"`
	SeverityValues  map[string]string   `json:"sv,omitempty"`
	LifecycleValues map[string]string   `json:"lv,omitempty"`
}

// NewMapping turns a proposal into a mapping for the shape it was made from.
// A value table keeps only entries whose field is an enum (or text holding a
// vocabulary word, such as "Not classified"), with keys in normValue form;
// of keys that normalize alike, the first in sorted order wins. The progress
// scale is decided here, once: percent when the key says so or the sample is
// above 1, ratio when the key says ratio or fraction or the sample is a
// fraction. A sample of exactly 0 or 1 decides nothing and gives percent,
// the more common scale.
func NewMapping(p Proposal, shapes []ShapeField) Mapping {
	byPath := make(map[string]*ShapeField, len(shapes))
	for i := range shapes {
		byPath[shapes[i].Path] = &shapes[i]
	}
	m := Mapping{V: MappingVersion, Kind: p.Kind, Paths: map[Role]string{}, Classes: map[Role]ValueClass{}}
	for _, r := range Roles {
		path := p.Path(r)
		if path == "" {
			continue
		}
		m.Paths[r] = path
		if f := byPath[path]; f != nil {
			m.Classes[r] = f.Class
		}
	}
	m.SeverityValues = table(p.SeverityValues, byPath[p.Severity], severityStates)
	m.LifecycleValues = table(p.LifecycleValues, byPath[p.Lifecycle], lifecycleStates)
	if p.Progress != "" {
		m.ProgressScale = progressScale(lastKey(p.Progress), byPath[p.Progress])
	}
	return m
}

func progressScale(key string, f *ShapeField) string {
	key = strings.ToLower(key)
	switch {
	case strings.Contains(key, "percent"), strings.Contains(key, "pct"):
		return ScalePercent
	case strings.Contains(key, "ratio"), strings.Contains(key, "fraction"):
		return ScaleRatio
	case f == nil || f.Range == "" || f.Range == RangeNeg || f.Range == RangeNegBig:
		return ScaleRatio
	case f.Range != Range0To1, f.Flags.Has(FlagInt):
		return ScalePercent
	}
	return ScaleRatio
}

var (
	severityStates  = []string{SeverityCritical, SeverityWarning, SeverityInfo}
	lifecycleStates = []string{LifecycleOngoing, LifecycleEnded, LifecycleUpdate}
)

func table(in map[string]string, f *ShapeField, states []string) map[string]string {
	if len(in) == 0 || f == nil {
		return nil
	}
	if f.Class != ClassEnum && f.Class != ClassText {
		return nil
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := map[string]string{}
	for _, raw := range keys {
		k := normValue(raw)
		if _, dup := out[k]; dup || k == "" || !slices.Contains(states, in[raw]) || (f.Class == ClassText && !vocab[k]) {
			continue
		}
		out[k] = in[raw]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func lastKey(path string) string {
	segs := segments(path)
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1]
}

// Shape is the serialized form of a payload's shapes.
type Shape struct {
	V         int          `json:"v"`
	Truncated bool         `json:"truncated,omitempty"`
	Fields    []ShapeField `json:"fields"`
}

// NewShape wraps shapes for serializing. Past MaxShapeBytes of json.Marshal
// output, fields are dropped from the end and Truncated is set; fields whose
// path is in keep (the mapped ones) are never dropped, so a mapping still
// validates against the shape. ProposeShapes on a truncated shape can
// differ from the proposal the full payload got.
func NewShape(shapes []ShapeField, truncated bool, keep ...string) Shape {
	s := Shape{V: ShapeVersion, Truncated: truncated, Fields: shapes}
	// The envelope with Truncated set, the brackets and the commas.
	total := len(`{"v":1,"truncated":true,"fields":[]}`) + len(shapes)
	sizes := make([]int, len(shapes))
	for i := range shapes {
		b, _ := json.Marshal(&shapes[i]) // ShapeField always encodes
		sizes[i] = len(b)
		total += sizes[i]
	}
	if total <= MaxShapeBytes {
		return s
	}
	drop := make([]bool, len(shapes))
	for i := len(shapes) - 1; i >= 0 && total > MaxShapeBytes; i-- {
		if !slices.Contains(keep, shapes[i].Path) {
			drop[i] = true
			total -= sizes[i] + 1
		}
	}
	s.Fields = make([]ShapeField, 0, len(shapes))
	for i := range shapes {
		if !drop[i] {
			s.Fields = append(s.Fields, shapes[i])
		}
	}
	s.Truncated = true
	return s
}

// allowedClasses is what a user may map to each role. A secret is allowed
// as a correlation id only when its value, not its key, makes it one: the id
// is hashed and never shown.
var allowedClasses = map[Role][]ValueClass{
	RoleTitle:       {ClassText, ClassEnum, ClassEmail, ClassEmpty},
	RoleBody:        {ClassText, ClassEnum, ClassEmail, ClassEmpty},
	RoleURL:         {ClassURL, ClassEmpty},
	RoleCorrelation: {ClassID, ClassEnum, ClassText, ClassNumber, ClassSecret, ClassEmpty},
	RoleProgress:    {ClassNumber, ClassEmpty},
	RoleSeverity:    {ClassEnum, ClassNumber, ClassText, ClassEmpty},
	RoleLifecycle:   {ClassEnum, ClassText, ClassEmpty},
}

// AllowedClasses lists the value classes a field may have to be mapped to a
// role. Severity and lifecycle take text for two-word values such as "Not
// classified" and "In Progress"; empty is allowed everywhere because one
// sample may simply lack the value.
func AllowedClasses(r Role) []ValueClass {
	return slices.Clone(allowedClasses[r])
}

// Mappable reports whether a field may fill a role: its class is
// allowed, a secret only by value and only as correlation, and progress comes
// from a JSON number (or a sample without a value).
func Mappable(r Role, f *ShapeField) bool {
	if !slices.Contains(allowedClasses[r], f.Class) {
		return false
	}
	if f.Class == ClassSecret && secretPath(f.Path) {
		return false
	}
	return r != RoleProgress || f.Type == TypeNumber || f.Class == ClassEmpty
}

// rankable is Mappable without what the heuristic should never propose: an
// empty sample, an email address.
func rankable(r Role, f *ShapeField) bool {
	return f.Class != ClassEmpty && f.Class != ClassEmail && Mappable(r, f)
}

// Validate checks a mapping against the shape it is meant for, without any
// sample values: every path exists and is Mappable to its role, title and
// body differ, and the value tables are small, keyed in normValue form, and
// map onto PushWard's own states. All problems are reported, in role order.
func (m Mapping) Validate(s Shape) error {
	var errs []error
	if m.V != MappingVersion {
		errs = append(errs, fmt.Errorf("mapping version %d, want %d", m.V, MappingVersion))
	}
	switch m.Kind {
	case KindNotification, KindAlert, KindProgress:
	default:
		errs = append(errs, fmt.Errorf("unknown kind %q", m.Kind))
	}
	for r := range m.Paths {
		if !slices.Contains(Roles, r) {
			errs = append(errs, fmt.Errorf("unknown role %q", r))
		}
	}
	byPath := make(map[string]*ShapeField, len(s.Fields))
	for i := range s.Fields {
		byPath[s.Fields[i].Path] = &s.Fields[i]
	}
	for _, r := range Roles {
		path := m.Paths[r]
		if path == "" {
			continue
		}
		f := byPath[path]
		switch {
		case f == nil:
			errs = append(errs, fmt.Errorf("%s: %q is not in the payload", r, path))
		case !Mappable(r, f):
			errs = append(errs, fmt.Errorf("%s: %q holds a %s %s, which cannot be the %s", r, path, f.Type, f.Class, r))
		}
	}
	if t := m.Paths[RoleTitle]; t != "" && t == m.Paths[RoleBody] {
		errs = append(errs, fmt.Errorf("title and body are both %q", t))
	}
	switch {
	case m.ProgressScale != "" && m.ProgressScale != ScaleRatio && m.ProgressScale != ScalePercent:
		errs = append(errs, fmt.Errorf("unknown progress scale %q", m.ProgressScale))
	case m.ProgressScale == "" && m.Paths[RoleProgress] != "":
		errs = append(errs, errors.New("progress is mapped without a scale"))
	}
	errs = append(errs, validateTable(RoleSeverity, m.SeverityValues, severityStates)...)
	errs = append(errs, validateTable(RoleLifecycle, m.LifecycleValues, lifecycleStates)...)
	return errors.Join(errs...)
}

func validateTable(r Role, t map[string]string, states []string) []error {
	var errs []error
	if len(t) > MaxTableEntries {
		errs = append(errs, fmt.Errorf("%s values: %d entries, at most %d", r, len(t), MaxTableEntries))
	}
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		switch {
		case k == "":
			errs = append(errs, fmt.Errorf("%s values: empty key", r))
		case utf8.RuneCountInString(k) > MaxTableKeyRunes:
			errs = append(errs, fmt.Errorf("%s values: key longer than %d characters", r, MaxTableKeyRunes))
		case k != normValue(k):
			errs = append(errs, fmt.Errorf("%s values: key %q is not normalized, want %q", r, k, normValue(k)))
		case !slices.Contains(states, t[k]):
			errs = append(errs, fmt.Errorf("%s values: %q maps to %q, want one of %s", r, k, t[k], strings.Join(states, ", ")))
		}
	}
	return errs
}

// NormValue is the form value-table keys are stored and looked up in:
// trimmed, lower case, spaces and hyphens as underscores.
func NormValue(v string) string {
	return normValue(v)
}
