package universal

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// The stored schema, v1. Changing these strings means a new ShapeVersion.
func TestShapeJSON(t *testing.T) {
	fields, _ := flatten(t, `{"alerts": [{"labels": {"severity": "critical"}, "values": {"A": 1}}]}`)
	want := []string{
		`{"p":"alerts[].labels.severity","t":"string","c":"enum","n":8,"w":1,"f":["enum","idtok","letters","lower"],"vx":"critical"}`,
		`{"p":"alerts[].values.A","t":"number","c":"number","n":1,"w":1,"f":["id","int","num"],"r":"0_1"}`,
	}
	for i, s := range ShapesOf(fields) {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want[i] {
			t.Errorf("shape %d =\n%s\nwant\n%s", i, b, want[i])
		}
	}
}

func TestShapeOf(t *testing.T) {
	cases := []struct {
		f     Field
		flags []string
		rng   string
		vx    string
		vl    string
	}{
		{Field{Path: "title", Value: "Disk almost full", Type: TypeString}, []string{"cap", "letters"}, "", "", ""},
		{Field{Path: "state", Value: "FIRING", Type: TypeString}, []string{"enum", "idtok", "letters", "upper"}, "", "firing", ""},
		{Field{Path: "state", Value: "Firing", Type: TypeString}, []string{"enum", "idtok", "letters"}, "", "firing", ""},
		{Field{Path: "event", Value: "alert.resolved", Type: TypeString}, []string{"enum", "idtok", "letters", "lower"}, "", "", "resolved"},
		{Field{Path: "event", Value: "sync-failed", Type: TypeString}, []string{"enum", "idtok", "letters", "lower"}, "", "", "failed"},
		{Field{Path: "state", Value: "not-ready", Type: TypeString}, []string{"enum", "idtok", "letters", "lower"}, "", "", ""},
		{Field{Path: "state", Value: "Not Resolved", Type: TypeString}, []string{"cap", "letters"}, "", "", ""},
		{Field{Path: "state", Value: "In Progress", Type: TypeString}, []string{"cap", "letters"}, "", "in_progress", ""},
		{Field{Path: "url", Value: "https://example.com/a", Type: TypeString}, []string{"idtok", "letters", "url"}, "", "", ""},
		{Field{Path: "at", Value: "2026-09-26T10:00:00Z", Type: TypeString}, []string{"idtok", "letters", "time"}, "", "", ""},
		{Field{Path: "id", Value: "3f2b8c1e9d", Type: TypeString}, []string{"id", "idtok", "letters"}, "", "", ""},
		{Field{Path: "id", Value: "12345", Type: TypeString}, []string{"id", "idtok", "num"}, "", "", ""},
		{Field{Path: "ratio", Value: "0.5", Type: TypeString}, []string{"idtok", "num"}, "", "", ""},
		{Field{Path: "n", Value: "NaN", Type: TypeString}, []string{"enum", "idtok", "letters"}, "", "", ""},
		{Field{Path: "pct", Value: "42.5", Type: TypeNumber}, []string{"num"}, Range10To1h, "", ""},
		{Field{Path: "n", Value: "-3", Type: TypeNumber}, []string{"id", "int", "num"}, RangeNeg, "", ""},
		{Field{Path: "n", Value: "-2e7", Type: TypeNumber}, []string{"letters", "num"}, RangeNegBig, "", ""},
		{Field{Path: "n", Value: "1", Type: TypeNumber}, []string{"id", "int", "num"}, Range0To1, "", ""},
		{Field{Path: "n", Value: "10", Type: TypeNumber}, []string{"id", "int", "num"}, Range1To10, "", ""},
		{Field{Path: "n", Value: "100", Type: TypeNumber}, []string{"id", "int", "num"}, Range10To1h, "", ""},
		{Field{Path: "n", Value: "1000000", Type: TypeNumber}, []string{"id", "int", "num"}, Range1hTo1M, "", ""},
		{Field{Path: "n", Value: "1000001", Type: TypeNumber}, []string{"id", "int", "num"}, RangeBig, "", ""},
		{Field{Path: "n", Value: "1e400", Type: TypeNumber}, []string{"letters"}, "", "", ""},
		{Field{Path: "ok", Value: "true", Type: TypeBool}, []string{"letters"}, "", "", ""},
		{Field{Path: "note", Value: "", Type: TypeNull}, nil, "", "", ""},
		{Field{Path: "password", Value: "open", Type: TypeString}, []string{"enum", "idtok", "letters", "lower"}, "", "", ""},
	}
	for _, c := range cases {
		s := ShapesOf([]Field{c.f})[0]
		var got []string
		for i, name := range flagNames {
			if s.Flags&(1<<i) != 0 {
				got = append(got, name)
			}
		}
		if !reflect.DeepEqual(got, c.flags) || s.Range != c.rng || s.Vocab != c.vx || s.LastWord != c.vl {
			t.Errorf("%q (%s): flags %v range %q vx %q vl %q, want %v %q %q %q",
				c.f.Value, c.f.Type, got, s.Range, s.Vocab, s.LastWord, c.flags, c.rng, c.vx, c.vl)
		}
		v := strings.TrimSpace(c.f.Value)
		if s.Runes != utf8.RuneCountInString(v) || s.Words != len(strings.Fields(v)) {
			t.Errorf("%q: n=%d w=%d", c.f.Value, s.Runes, s.Words)
		}
	}
}

func TestFlagsJSON(t *testing.T) {
	var f Flags
	if err := json.Unmarshal([]byte(`["url","enum","later_flag"]`), &f); err != nil {
		t.Fatal(err)
	}
	if f != FlagURL|FlagEnum {
		t.Errorf("flags = %b, want url and enum with the unknown name ignored", f)
	}
	b, _ := json.Marshal(f)
	if string(b) != `["enum","url"]` {
		t.Errorf("marshal = %s, want the names sorted", b)
	}
}

func TestTokensOf(t *testing.T) {
	got := TokensOf("alerts[].commonLabels.*.alertName")
	want := PathTokens{Key: []string{"alert", "name"}, Parents: []string{"alerts", "common", "labels"}, LastParent: []string{"common", "labels"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TokensOf = %+v, want %+v", got, want)
	}
	if b, _ := json.Marshal(TokensOf("[]")); string(b) != `{"k":[],"p":[],"lp":[]}` {
		t.Errorf("empty path tokens = %s", b)
	}
}

// relayFixtures flattens every JSON file under relay/testdata, keyed by its
// path relative to that directory.
func relayFixtures(t *testing.T) map[string][]Field {
	t.Helper()
	const root = "../../testdata"
	var names []string
	err := filepath.WalkDir(root, func(name string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(name) == ".json" {
			names = append(names, name)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 80 {
		t.Fatalf("found %d fixtures, want the whole relay/testdata tree", len(names))
	}
	out := make(map[string][]Field, len(names))
	for _, name := range names {
		body, err := os.ReadFile(name) // #nosec G304 -- fixture path under relay/testdata
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.ToSlash(rel)], _ = flatten(t, string(body))
	}
	return out
}

// Every relay fixture proposes and ranks the same from its stored shapes as
// from its payload, its proposal validates, and the stored shapes carry no
// sample text beyond the vocabulary words. Propose wraps ProposeShapes, so
// this only tests that shapes survive JSON; TestHeuristicGolden is the check
// against the heuristic as it was before shapes.
func TestShapesRoundTrip(t *testing.T) {
	for name, fields := range relayFixtures(t) {
		shapes := ShapesOf(fields)
		b, err := json.Marshal(NewShape(shapes, false))
		if err != nil {
			t.Fatal(err)
		}
		var stored Shape
		if err := json.Unmarshal(b, &stored); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(stored.Fields, shapes) {
			t.Errorf("%s: shapes change through JSON", name)
		}
		if got, want := ProposeShapes(stored.Fields), Propose(fields); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: ProposeShapes =\n%+v\nPropose =\n%+v", name, got, want)
		}
		for _, r := range Roles {
			if got, want := RankShapes(stored.Fields, r), Rank(fields, r); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: RankShapes(%s) differs from Rank", name, r)
			}
		}
		if err := NewMapping(Propose(fields), shapes).Validate(stored); err != nil {
			t.Errorf("%s: the heuristic's mapping does not validate: %v", name, err)
		}
		for i, f := range fields {
			v := strings.TrimSpace(f.Value)
			if shapes[i].Vocab != "" || utf8.RuneCountInString(v) < 8 {
				continue
			}
			s := shapes[i]
			s.Path = ""
			if b, _ := json.Marshal(s); strings.Contains(string(b), v) {
				t.Errorf("%s: the shape of %s carries its value: %s", name, f.Path, b)
			}
		}
	}
}
