package ranker

import (
	"encoding/json"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
)

func shapesOf(t testing.TB, body string) []universal.ShapeField {
	t.Helper()
	fields, _, err := universal.Flatten(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return universal.ShapesOf(fields)
}

// relayFixtures reads every provider fixture under relay/testdata, keyed by
// its path there. The universal route's own payloads are left out, as the
// golden leaves them out.
func relayFixtures(t testing.TB) map[string][]byte {
	t.Helper()
	const root = "../../../testdata"
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
	out := map[string][]byte{}
	for _, name := range names {
		rel, err := filepath.Rel(root, name)
		if err != nil {
			t.Fatal(err)
		}
		if rel = filepath.ToSlash(rel); strings.HasPrefix(rel, "universal/") {
			continue
		}
		if out[rel], err = os.ReadFile(name); err != nil { // #nosec G304 -- fixture path under relay/testdata
			t.Fatal(err)
		}
	}
	if len(out) < 80 {
		t.Fatalf("found %d fixtures, want the whole relay/testdata tree", len(out))
	}
	return out
}

func option(opts []Option, path string) *Option {
	for i := range opts {
		if opts[i].Path == path {
			return &opts[i]
		}
	}
	return nil
}

func featureMap(fs Features) map[string]float64 {
	m := make(map[string]float64, len(fs))
	for _, f := range fs {
		m[f.Name] = f.Value
	}
	return m
}

func roleIndex(r universal.Role) int {
	for i, x := range universal.Roles {
		if x == r {
			return i
		}
	}
	panic("unknown role " + r)
}

func TestExtractFeatures(t *testing.T) {
	shapes := shapesOf(t, `{"alert": {"title": "Disk almost full", "status": "firing", "data": {"data": {"id": "abc123def"}}},
		"items": [{"alertName": "x"}], "pct": 42}`)
	ex := Extract(shapes)

	title := ex.Options[roleIndex(universal.RoleTitle)]
	got := featureMap(option(title, "alert.title").Features)
	want := map[string]float64{
		"t:string": 1, "c:text": 1, "n:5": 1, "w:2": 1, "f:cap": 1, "f:letters": 1,
		"depth": 2, "order": 0, "k:title": 1, "p:alert": 1, "lp:alert": 1,
		"h:rank0": 1, "h:gap": 0,
	}
	for name, v := range want {
		if got[name] != v {
			t.Errorf("alert.title %s = %v, want %v (all: %v)", name, got[name], v, got)
		}
	}
	if _, ok := got["h:score"]; !ok {
		t.Error("alert.title has no h:score")
	}
	for name := range got {
		if strings.HasPrefix(name, "claimed:") {
			t.Errorf("alert.title is the heuristic's title, yet has %s", name)
		}
	}
	if st := featureMap(option(title, "alert.status").Features); st["claimed:lifecycle"] != 1 || st["vx"] != 1 {
		t.Errorf("alert.status as a title option = %v, want claimed:lifecycle and vx", st)
	}

	// A repeated parent word counts once; an array member is in_array.
	corr := featureMap(option(ex.Options[roleIndex(universal.RoleCorrelation)], "alert.data.data.id").Features)
	if corr["p:data"] != 1 || corr["lp:data"] != 1 || corr["depth"] != 4 || corr["k:id"] != 1 {
		t.Errorf("alert.data.data.id features = %v", corr)
	}
	n := 0
	for _, f := range option(ex.Options[roleIndex(universal.RoleCorrelation)], "alert.data.data.id").Features {
		if f.Name == "p:data" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("p:data appears %d times", n)
	}
	arr := featureMap(option(title, "items[].alertName").Features)
	if arr["in_array"] != 1 || arr["k:alert"] != 1 || arr["k:name"] != 1 || arr["p:items"] != 1 {
		t.Errorf("items[].alertName features = %v", arr)
	}
	prog := featureMap(option(ex.Options[roleIndex(universal.RoleProgress)], "pct").Features)
	if prog["t:number"] != 1 || prog["r:10_100"] != 1 || prog["f:int"] != 1 || prog["f:num"] != 1 {
		t.Errorf("pct features = %v", prog)
	}

	for i, opts := range ex.Options {
		none := opts[len(opts)-1]
		if none.Path != "" {
			t.Fatalf("%s: last option is %q, want none", universal.Roles[i], none.Path)
		}
		nf := featureMap(none.Features)
		if nf["none"] != 1 || len(nf) != 3 {
			t.Errorf("%s: none features = %v", universal.Roles[i], nf)
		}
		if len(opts)-1 > TopK || len(opts)-1 != min(len(ex.Ranked[i]), TopK) {
			t.Errorf("%s: %d options for %d candidates", universal.Roles[i], len(opts)-1, len(ex.Ranked[i]))
		}
		if len(ex.Ranked[i]) == 0 && nf["none:best"] != -5 {
			t.Errorf("%s: none:best without candidates = %v", universal.Roles[i], nf["none:best"])
		}
	}

	kind := featureMap(ex.Kind)
	if kind["hk:alert"] != 1 || kind["has:title"] != 1 || kind["has:lifecycle"] != 1 || kind["lifecycle_values:ongoing"] != 1 {
		t.Errorf("kind features = %v", kind)
	}
}

// The features come from shapes alone: a shape stored as JSON and read back
// gives the same features as the payload it was made from, on every relay
// fixture.
func TestExtractStoredShapes(t *testing.T) {
	for name, body := range relayFixtures(t) {
		shapes := shapesOf(t, string(body))
		b, err := json.Marshal(universal.NewShape(shapes, false))
		if err != nil {
			t.Fatal(err)
		}
		var stored universal.Shape
		if err := json.Unmarshal(b, &stored); err != nil {
			t.Fatal(err)
		}
		if got, want := Extract(stored.Fields), Extract(shapes); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: features differ between the payload and its stored shape", name)
		}
	}
}

func TestFeaturesJSON(t *testing.T) {
	b, err := json.Marshal(Features{{"z", 1}, {"a", 0.5}, {"k:\"q\"", -5}, {"n", 1e-7}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"z":1,"a":0.5,"k:\"q\"":-5,"n":1e-07}`; string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
	var back map[string]float64
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back["n"] != 1e-7 {
		t.Errorf("round trip lost precision: %v", back)
	}

	// Names are JSON strings, whatever runes a key word holds.
	names := []string{"k:a\x01b", "k:\u007f", "p:<&>", "k:stra\u00dfe", "lp:\u2028", "k:back\\slash", "k:\ttab"}
	fs := make(Features, len(names))
	for i, n := range names {
		fs[i] = Feature{n, float64(i)}
	}
	b, err = json.Marshal(fs)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b) {
		t.Fatalf("invalid JSON: %s", b)
	}
	back = nil
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	for i, n := range names {
		if v, ok := back[n]; !ok || v != float64(i) {
			t.Errorf("%q did not survive: %s", n, b)
		}
	}
	if _, err := json.Marshal(Features{{"x", math.NaN()}}); err == nil {
		t.Error("a NaN feature marshaled")
	}
}
