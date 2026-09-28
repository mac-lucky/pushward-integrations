package presets

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// want is what Apply must make of one fixture in testdata/. Source is the
// ?source= value it is posted with; corr is the raw correlation value.
type want struct {
	preset, source      string
	title, body, url    string
	corr, sev, life     string
	sevTable, lifeTable bool
}

// fixtures maps a file in testdata/ to what Apply must make of it. Each
// vendor group registers its own from an init in fixtures_<group>_test.go.
// Severity and lifecycle are checked as looked up in the preset's own
// tables unless the fixture says otherwise.
var fixtures = map[string]want{}

func addFixtures(m map[string]want) {
	for name, w := range m {
		if _, dup := fixtures[name]; dup {
			panic("fixture " + name + " registered twice")
		}
		fixtures[name] = w
	}
}

func readFields(t *testing.T, path string) []universal.Field {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- test payloads under a directory the test names
	if err != nil {
		t.Fatal(err)
	}
	fields, _, err := universal.Flatten(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return fields
}

func paths(fields []universal.Field) map[string]bool {
	have := make(map[string]bool, len(fields))
	for _, f := range fields {
		have[f.Path] = true
	}
	return have
}

func byID(id string) *Preset {
	for i := range all {
		if all[i].ID == id {
			return &all[i]
		}
	}
	return nil
}

func TestLoad(t *testing.T) {
	if len(all) == 0 {
		t.Fatal("no presets loaded")
	}
	ids := map[string]bool{}
	for _, p := range all {
		if ids[p.ID] {
			t.Errorf("duplicate id %q", p.ID)
		}
		ids[p.ID] = true
		for _, table := range []map[string]string{p.mapping.SeverityValues, p.mapping.LifecycleValues} {
			for k := range table {
				if k != universal.NormValue(k) {
					t.Errorf("%s: table key %q is not in NormValue form", p.ID, k)
				}
			}
		}
		if p.Kind != p.mapping.Kind {
			t.Errorf("%s: kind %q, mapping says %q", p.ID, p.Kind, p.mapping.Kind)
		}
	}
}

func TestLoadRejects(t *testing.T) {
	const good = `{"id":"x","vendor":"X","version":1,"sources":["x"],"require":["a","b","s"],"forbid":[],"alias_only":false,
		"mapping":{"v":1,"k":"alert","p":{"title":"a","correlation":"b","lifecycle":"s"},"lv":{"open":"ongoing","closed":"ended"}},
		"doc":"https://example.com/docs"}`
	if _, err := load(fstest.MapFS{"data/x.json": {Data: []byte(good)}}); err != nil {
		t.Fatalf("the base case does not load: %v", err)
	}
	cases := map[string]struct{ from, to string }{
		"unknown field":         {`"doc":`, `"extra":1,"doc":`},
		"unknown role":          {`"lifecycle":"s"}`, `"lifecycle":"s","colour":"c"}`},
		"unknown kind":          {`"k":"alert"`, `"k":"banner"`},
		"unknown state":         {`"closed":"ended"`, `"closed":"gone"`},
		"key not normalized":    {`"closed":"ended"`, `"Closed Out":"ended"`},
		"title not required":    {`"title":"a"`, `"title":"t"`},
		"no title":              {`"title":"a",`, ``},
		"alias only, no source": {`"sources":["x"],"require":["a","b","s"],"forbid":[],"alias_only":false`, `"sources":[],"require":["a","b","s"],"forbid":[],"alias_only":true`},
		"alert, no correlation": {`"correlation":"b",`, ``},
		"alert, never ends":     {`,"closed":"ended"`, ``},
		"corr not required":     {`"correlation":"b"`, `"correlation":"z"`},
		"life not required":     {`"lifecycle":"s"}`, `"lifecycle":"z"}`},
		"required and forbid":   {`"forbid":[]`, `"forbid":["b"]`},
		"id is not file name":   {`"id":"x"`, `"id":"y"`},
		"plain http doc":        {`https://example.com`, `http://example.com`},
		"table without path":    {`"lv":{`, `"sv":{"high":"critical"},"lv":{`},
		"classes":               {`"lv":{`, `"c":{"title":"text"},"lv":{`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			bad := strings.Replace(good, c.from, c.to, 1)
			if bad == good {
				t.Fatalf("%q is not in the base case", c.from)
			}
			if _, err := load(fstest.MapFS{"data/x.json": {Data: []byte(bad)}}); err == nil {
				t.Error("loaded")
			}
		})
	}
}

func fixtureFiles(t *testing.T) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestFixtures(t *testing.T) {
	names := fixtureFiles(t)
	covered := map[string]bool{}
	for _, name := range names {
		base := filepath.Base(name)
		w, ok := fixtures[base]
		if !ok {
			t.Errorf("%s has no expectation", base)
			continue
		}
		t.Run(base, func(t *testing.T) {
			if !strings.HasPrefix(base, w.preset) {
				t.Errorf("fixture %s is not named after its preset %s", base, w.preset)
			}
			fields := readFields(t, name)
			p, m, ok := Match(w.source, fields)
			if !ok || p.ID != w.preset {
				t.Fatalf("Match = %q, %v; want %q", p.ID, ok, w.preset)
			}
			covered[p.ID] = true
			if err := m.Validate(universal.NewShape(universal.ShapesOf(fields), false)); err != nil {
				t.Errorf("mapping does not validate against the fixture: %v", err)
			}
			ev := universal.Apply(m, fields, w.source)
			if ev.Kind != p.Kind {
				t.Errorf("kind = %q, want %q", ev.Kind, p.Kind)
			}
			check := func(role universal.Role, got, want string) {
				t.Helper()
				if m.Paths[role] == "" && want != "" {
					t.Errorf("%s: not mapped, want %q", role, want)
				}
				if got != want {
					t.Errorf("%s = %q, want %q", role, got, want)
				}
			}
			check(universal.RoleTitle, ev.Title, w.title)
			if m.Paths[universal.RoleBody] != "" || w.body != "" {
				check(universal.RoleBody, ev.Body, w.body)
			}
			check(universal.RoleURL, ev.URL, w.url)
			wantKey := ""
			if w.corr != "" {
				wantKey = text.HashHex(w.corr, 16)
			}
			check(universal.RoleCorrelation, ev.CorrelationKey, wantKey)
			if m.Paths[universal.RoleSeverity] != "" || w.sev != "" {
				check(universal.RoleSeverity, ev.Severity, w.sev)
				if from := ev.SevFrom; from != universal.FromTable && !w.sevTable {
					t.Errorf("severity came from %s, not the table", from)
				}
			}
			if m.Paths[universal.RoleLifecycle] != "" || w.life != "" {
				check(universal.RoleLifecycle, ev.Lifecycle, w.life)
				if from := ev.LcFrom; from != universal.FromTable && !w.lifeTable {
					t.Errorf("lifecycle came from %s, not the table", from)
				}
			}
		})
	}
	for _, p := range all {
		if !covered[p.ID] {
			t.Errorf("%s has no fixture", p.ID)
		}
	}
}

// TestEndsWhatItOpened checks that every alert and progress preset has an
// ended fixture sharing its correlation with an ongoing one.
func TestEndsWhatItOpened(t *testing.T) {
	type state struct{ ongoing, ended map[string]bool }
	byPreset := map[string]*state{}
	for base, w := range fixtures {
		s := byPreset[w.preset]
		if s == nil {
			s = &state{map[string]bool{}, map[string]bool{}}
			byPreset[w.preset] = s
		}
		switch w.life {
		case universal.LifecycleOngoing:
			s.ongoing[w.corr] = true
		case universal.LifecycleEnded:
			s.ended[w.corr] = true
		}
		if w.corr == "" && w.life != "" {
			t.Errorf("%s: a lifecycle without a correlation value", base)
		}
	}
	for _, p := range all {
		if p.Kind == universal.KindNotification {
			continue
		}
		s := byPreset[p.ID]
		if s == nil {
			continue // TestFixtures reports it
		}
		paired := false
		for c := range s.ended {
			paired = paired || s.ongoing[c]
		}
		if !paired {
			t.Errorf("%s: no ended fixture with the correlation of an ongoing one", p.ID)
		}
	}
}

// TestExclusive checks that no preset of another vendor could claim a
// fixture, whatever the tie-break, and that a preset that needs no alias
// matches its fixtures without one.
func TestExclusive(t *testing.T) {
	for _, name := range fixtureFiles(t) {
		base := filepath.Base(name)
		w, ok := fixtures[base]
		if !ok {
			continue
		}
		fields := readFields(t, name)
		own := byID(w.preset)
		if own == nil {
			t.Errorf("%s: no preset %q", base, w.preset)
			continue
		}
		for _, c := range candidates(w.source, paths(fields)) {
			if c.Vendor != own.Vendor {
				t.Errorf("%s: %s (%s) is a candidate too", base, c.ID, c.Vendor)
			}
		}
		if !own.aliasOnly {
			if p, _, ok := Match("", fields); !ok || p.ID != own.ID {
				t.Errorf("%s: without a source Match = %q, %v", base, p.ID, ok)
			}
		} else if p, _, ok := Match("", fields); ok {
			t.Errorf("%s: alias-only fixture matches %s without a source", base, p.ID)
		}
	}
}

// providerFixtures is the relay's own testdata: payloads that dedicated
// providers handle. None may match a preset, except the Alertmanager
// payloads the universal provider keeps for itself.
func TestProviderFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "..", "testdata")
	n := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		n++
		rel, _ := filepath.Rel(root, path)
		dir := filepath.Dir(rel)
		fields := readFields(t, path)
		wantID := ""
		if dir == "universal" && strings.HasPrefix(filepath.Base(rel), "alertmanager_") {
			wantID = "alertmanager"
		}
		for _, source := range []string{"", dir} {
			p, _, ok := Match(source, fields)
			switch {
			case wantID == "" && ok:
				t.Errorf("%s (source %q) matches %s", rel, source, p.ID)
			case wantID != "" && (!ok || p.ID != wantID):
				t.Errorf("%s (source %q): Match = %q, %v; want %s", rel, source, p.ID, ok, wantID)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no provider fixtures found")
	}
}

func TestMatchOrder(t *testing.T) {
	saved := all
	t.Cleanup(func() { all = saved })
	mk := func(id string, alias bool, sources []string, require ...string) Preset {
		return Preset{
			ID: id, Vendor: id, Version: 1, Kind: universal.KindNotification,
			sources: sources, require: require, aliasOnly: alias,
			mapping: universal.Mapping{V: universal.MappingVersion, Kind: universal.KindNotification, Paths: map[universal.Role]string{universal.RoleTitle: "title"}},
		}
	}
	all = []Preset{
		mk("b-short", false, []string{"b"}, "title"),
		mk("a-short", false, []string{"a"}, "title"),
		mk("long", false, nil, "title", "extra"),
		mk("alias", true, []string{"svc"}, "title"),
	}
	fields := []universal.Field{{Path: "title", Value: "Disk full", Type: universal.TypeString}, {Path: "extra", Value: "x", Type: universal.TypeString}}
	for _, c := range []struct{ source, want string }{
		{"", "long"},
		{"svc", "alias"},
		{"SVC", "alias"},
		{"b", "b-short"},
		{"other", "long"},
	} {
		if p, _, _ := Match(c.source, fields); p.ID != c.want {
			t.Errorf("source %q: Match = %q, want %q", c.source, p.ID, c.want)
		}
	}
	if p, _, _ := Match("", fields[:1]); p.ID != "a-short" {
		t.Errorf("tie on require count: Match = %q, want a-short", p.ID)
	}
}

func TestMatchDropsAbsentRoles(t *testing.T) {
	saved := all
	t.Cleanup(func() { all = saved })
	all = []Preset{{
		ID: "p", Vendor: "P", Version: 1, Kind: universal.KindAlert, require: []string{"name"},
		mapping: universal.Mapping{
			V: universal.MappingVersion, Kind: universal.KindAlert,
			Paths: map[universal.Role]string{
				universal.RoleTitle: "name", universal.RoleCorrelation: "id",
				universal.RoleSeverity: "level", universal.RoleLifecycle: "state",
			},
			SeverityValues:  map[string]string{"sev_a": universal.SeverityCritical},
			LifecycleValues: map[string]string{"open": universal.LifecycleOngoing, "shut": universal.LifecycleEnded},
		},
	}}
	fields := []universal.Field{
		{Path: "name", Value: "Disk full", Type: universal.TypeString},
		{Path: "state", Value: "shut", Type: universal.TypeString},
	}
	_, m, ok := Match("", fields)
	if !ok {
		t.Fatal("no match")
	}
	if _, has := m.Paths[universal.RoleCorrelation]; has || m.SeverityValues != nil {
		t.Errorf("absent roles kept: %+v", m)
	}
	if m.LifecycleValues["shut"] != universal.LifecycleEnded {
		t.Errorf("lifecycle table lost: %+v", m.LifecycleValues)
	}
	m.LifecycleValues["shut"] = universal.LifecycleOngoing
	if all[0].mapping.LifecycleValues["shut"] != universal.LifecycleEnded {
		t.Error("the returned table aliases the preset's")
	}
	// A title that is not mappable (a link) fails validation: no match.
	fields[0] = universal.Field{Path: "name", Value: "https://example.com/x", Type: universal.TypeString}
	if _, _, ok := Match("", fields); ok {
		t.Error("matched with a link as the title")
	}
}
