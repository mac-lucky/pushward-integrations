// Package presets holds fixed mappings for webhook payloads whose vendor
// documents their shape. A preset is picked by the paths a payload has, not
// by its values, so the same shape always maps the same way; the heuristic
// only sees what no preset claims.
package presets

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
)

// Preset is one vendor payload shape and the mapping used for it.
type Preset struct {
	ID      string
	Vendor  string
	Version int
	Kind    universal.Kind

	sources   []string
	require   []string
	forbid    []string
	aliasOnly bool
	mapping   universal.Mapping
}

// file is a preset as written in data/<id>.json. Doc is the vendor page its
// paths were checked against.
type file struct {
	ID        string            `json:"id"`
	Vendor    string            `json:"vendor"`
	Version   int               `json:"version"`
	Sources   []string          `json:"sources"`
	Require   []string          `json:"require"`
	Forbid    []string          `json:"forbid"`
	AliasOnly bool              `json:"alias_only"`
	Mapping   universal.Mapping `json:"mapping"`
	Doc       string            `json:"doc"`
}

//go:embed data/*.json
var data embed.FS

var all = mustLoad()

func mustLoad() []Preset {
	ps, err := load(data)
	if err != nil {
		panic(err)
	}
	return ps
}

// All returns every preset, sorted by id.
func All() []Preset {
	return slices.Clone(all)
}

// load reads data/*.json from fsys and checks every preset without a
// payload: the checks that need one are Match's.
func load(fsys fs.FS) ([]Preset, error) {
	names, err := fs.Glob(fsys, "data/*.json")
	if err != nil {
		return nil, err
	}
	var errs []error
	out := make([]Preset, 0, len(names))
	for _, name := range names {
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		// Naming the file after the id keeps ids unique.
		p, err := parse(b)
		if err == nil && p.ID+".json" != path.Base(name) {
			err = fmt.Errorf("id %q does not match the file name", p.ID)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("presets: %s: %w", name, err))
			continue
		}
		out = append(out, p)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Preset) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func parse(b []byte) (Preset, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f file
	if err := dec.Decode(&f); err != nil {
		return Preset{}, err
	}
	if err := f.check(); err != nil {
		return Preset{}, err
	}
	return Preset{
		ID: f.ID, Vendor: f.Vendor, Version: f.Version, Kind: f.Mapping.Kind,
		sources: f.Sources, require: f.Require, forbid: f.Forbid, aliasOnly: f.AliasOnly,
		mapping: f.Mapping,
	}, nil
}

func (f *file) check() error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	m := &f.Mapping
	if f.ID == "" || f.Vendor == "" || f.Version < 1 {
		fail("id, vendor and a version of 1 or more are required")
	}
	if !strings.HasPrefix(f.Doc, "https://") {
		fail("doc %q is not an https link", f.Doc)
	}
	if f.AliasOnly && len(f.Sources) == 0 {
		fail("alias_only without sources")
	}
	for _, s := range f.Sources {
		if s == "" || s != strings.ToLower(s) {
			fail("source %q is not a lower case name", s)
		}
	}
	if len(f.Require) == 0 {
		fail("no require paths")
	}
	for _, p := range f.Forbid {
		if slices.Contains(f.Require, p) {
			fail("%q is both required and forbidden", p)
		}
	}
	if len(m.Classes) > 0 {
		fail("a preset has no sample, so no classes")
	}
	// Every path as an empty sample is mappable to any role, so this checks
	// the rest: version, kind, roles, title against body, the progress
	// scale and the value tables.
	var shape universal.Shape
	for _, p := range m.Paths {
		shape.Fields = append(shape.Fields, universal.ShapeField{Path: p, Type: universal.TypeNull, Class: universal.ClassEmpty})
	}
	if err := m.Validate(shape); err != nil {
		errs = append(errs, err)
	}
	if t := m.Paths[universal.RoleTitle]; t == "" || !slices.Contains(f.Require, t) {
		fail("the title path %q must be required", t)
	}
	if len(m.SeverityValues) > 0 && m.Paths[universal.RoleSeverity] == "" {
		fail("severity values without a severity path")
	}
	if len(m.LifecycleValues) > 0 && m.Paths[universal.RoleLifecycle] == "" {
		fail("lifecycle values without a lifecycle path")
	}
	if m.Kind == universal.KindAlert || m.Kind == universal.KindProgress {
		if m.Paths[universal.RoleCorrelation] == "" || m.Paths[universal.RoleLifecycle] == "" {
			fail("a %s preset maps correlation and lifecycle", m.Kind)
		}
		// Required, so Match never drops them: a card with no key or no end
		// would pile up on the lock screen.
		for _, r := range []universal.Role{universal.RoleCorrelation, universal.RoleLifecycle} {
			if p := m.Paths[r]; p != "" && !slices.Contains(f.Require, p) {
				fail("the %s path %q must be required", r, p)
			}
		}
		// Only an end is needed: a null value, such as a run's conclusion
		// before it finishes, is ongoing, and one the table lacks is read the
		// way the heuristic's vocabulary reads it.
		if !slices.Contains(slices.Collect(maps.Values(m.LifecycleValues)), universal.LifecycleEnded) {
			fail("a %s preset's lifecycle values need an %s one", m.Kind, universal.LifecycleEnded)
		}
	}
	return errors.Join(errs...)
}

// Match finds the preset for a payload. A candidate has every required path
// and no forbidden one, and an alias-only preset is a candidate only when
// source is one of its names. Candidates are tried by source match, then by
// the number of required paths, then by id, and the first with a title path
// in this payload wins. Only paths decide: a value never turns a preset
// away, or one event of an alert could match where the next did not and
// leave its card open. The mapping comes back as written, value tables
// included, less the roles this payload lacks and the optional ones whose
// value does not fit them (a link as a body, words as a progress).
func Match(source string, fields []universal.Field) (Preset, universal.Mapping, bool) {
	return MatchShapes(source, fields, universal.ShapesOf(fields))
}

// MatchShapes is Match for a caller that has the payload's shapes already;
// shapes must be ShapesOf(fields).
func MatchShapes(source string, fields []universal.Field, shapes []universal.ShapeField) (Preset, universal.Mapping, bool) {
	have := make(map[string]bool, len(fields))
	for _, f := range fields {
		have[f.Path] = true
	}
	for _, p := range candidates(source, have) {
		m := p.trim(have)
		if m.Paths[universal.RoleTitle] == "" {
			continue
		}
		dropUnfit(&m, shapes)
		return *p, m, true
	}
	return Preset{}, universal.Mapping{}, false
}

// optional are the roles a payload's value may take away from a preset: the
// event reads the same without them. Title, correlation and lifecycle stay,
// whatever they hold, so every event of one alert keys and ends one card.
var optional = []universal.Role{universal.RoleBody, universal.RoleURL, universal.RoleSeverity, universal.RoleProgress}

// dropUnfit drops the optional roles whose field in this payload is not
// Mappable to them.
func dropUnfit(m *universal.Mapping, shapes []universal.ShapeField) {
	for _, r := range optional {
		path := m.Paths[r]
		if path == "" {
			continue
		}
		i := slices.IndexFunc(shapes, func(s universal.ShapeField) bool { return s.Path == path })
		if i >= 0 && universal.Mappable(r, &shapes[i]) {
			continue
		}
		delete(m.Paths, r)
		switch r {
		case universal.RoleSeverity:
			m.SeverityValues = nil
		case universal.RoleProgress:
			m.ProgressScale = ""
		}
	}
}

func candidates(source string, have map[string]bool) []*Preset {
	var out []*Preset
	for i := range all {
		p := &all[i]
		if p.aliasOnly && !p.named(source) || !p.fits(have) {
			continue
		}
		out = append(out, p)
	}
	slices.SortStableFunc(out, func(a, b *Preset) int {
		if an, bn := a.named(source), b.named(source); an != bn {
			if an {
				return -1
			}
			return 1
		}
		if d := len(b.require) - len(a.require); d != 0 {
			return d
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func (p *Preset) named(source string) bool {
	return source != "" && slices.ContainsFunc(p.sources, func(s string) bool { return strings.EqualFold(s, source) })
}

func (p *Preset) fits(have map[string]bool) bool {
	for _, r := range p.require {
		if !have[r] {
			return false
		}
	}
	for _, f := range p.forbid {
		if have[f] {
			return false
		}
	}
	return true
}

// trim copies the preset's mapping without the roles whose path this
// payload lacks, and without the tables and scale of the dropped ones.
func (p *Preset) trim(have map[string]bool) universal.Mapping {
	m := p.mapping
	m.SeverityValues = maps.Clone(m.SeverityValues)
	m.LifecycleValues = maps.Clone(m.LifecycleValues)
	m.Paths = make(map[universal.Role]string, len(p.mapping.Paths))
	for r, path := range p.mapping.Paths {
		if have[path] {
			m.Paths[r] = path
		}
	}
	if m.Paths[universal.RoleSeverity] == "" {
		m.SeverityValues = nil
	}
	if m.Paths[universal.RoleLifecycle] == "" {
		m.LifecycleValues = nil
	}
	if m.Paths[universal.RoleProgress] == "" {
		m.ProgressScale = ""
	}
	return m
}
