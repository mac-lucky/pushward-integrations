package ranker

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
)

// goldenTolerance bounds how far a probability or logit may drift from the
// Python scorer that wrote the golden from the same weights.json.
const goldenTolerance = 1e-6

type goldenRecord struct {
	Meta *struct {
		Weights         string `json:"weights"`
		FeaturesVersion int    `json:"features_version"`
		TopK            int    `json:"top_k"`
	} `json:"meta"`
	ID      string          `json:"id"`
	Fixture string          `json:"fixture"`
	Payload json.RawMessage `json:"payload"`
	Roles   map[universal.Role]struct {
		Options []string  `json:"options"`
		Logits  []float64 `json:"logits"`
		Probs   []float64 `json:"probs"`
	} `json:"roles"`
	Kind struct {
		Classes []universal.Kind `json:"classes"`
		Logits  []float64        `json:"logits"`
		Probs   []float64        `json:"probs"`
	} `json:"kind"`
	Mapping map[string]string `json:"mapping"`
}

// TestGolden scores the relay fixtures and a set of synthetic payloads and
// compares every option, logit, probability and the served mapping with
// testdata/golden.jsonl, which `just retrain` in pushward-classifier writes
// from the same weights.json with a Python scorer. weights.json and the
// golden are always replaced together.
func TestGolden(t *testing.T) {
	n := checkGolden(t, "testdata/golden.jsonl")
	if n < 80 {
		t.Errorf("golden has %d records, want the relay fixtures and the edge cases", n)
	}
}

// TestGoldenFull runs the same comparison on a larger golden that
// `just retrain` keeps outside this repo (OOD and corpus payloads), when
// UNIVERSAL_GOLDEN_FULL names it.
func TestGoldenFull(t *testing.T) {
	path := os.Getenv("UNIVERSAL_GOLDEN_FULL")
	if path == "" {
		t.Skip("UNIVERSAL_GOLDEN_FULL not set")
	}
	checkGolden(t, path)
}

func checkGolden(t *testing.T, path string) int {
	t.Helper()
	r := embedded(t)
	f, err := os.Open(path) // #nosec G304 G703 -- a test golden, or the path UNIVERSAL_GOLDEN_FULL names
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	n := 0
	for line := 1; sc.Scan(); line++ {
		var g goldenRecord
		if err := json.Unmarshal(sc.Bytes(), &g); err != nil {
			t.Fatalf("%s:%d: %v", path, line, err)
		}
		if g.Meta != nil {
			if g.Meta.Weights != r.id || g.Meta.FeaturesVersion != FeaturesVersion || g.Meta.TopK != TopK {
				t.Fatalf("%s is for weights %q (features v%d, top %d); embedded are %q (v%d, top %d): "+
					"copy weights.json and the golden from the same retrain", path, g.Meta.Weights,
					g.Meta.FeaturesVersion, g.Meta.TopK, r.id, FeaturesVersion, TopK)
			}
			continue
		}
		body := []byte(g.Payload)
		if g.Fixture != "" {
			body, err = os.ReadFile(filepath.Join("../../../testdata", filepath.FromSlash(g.Fixture))) // #nosec G304 -- fixture path under relay/testdata
			if err != nil {
				t.Errorf("%s: %v", g.ID, err)
				continue
			}
		}
		checkRecord(t, r, &g, body)
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

func checkRecord(t *testing.T, r *Ranker, g *goldenRecord, body []byte) {
	t.Helper()
	fields, _, err := universal.Flatten(strings.NewReader(string(body)))
	if err != nil {
		t.Errorf("%s: %v", g.ID, err)
		return
	}
	s := r.score(Extract(universal.ShapesOf(fields)))
	for i, role := range universal.Roles {
		want := g.Roles[role]
		opts := s.ex.Options[i]
		paths := make([]string, len(opts))
		for j := range opts {
			paths[j] = opts[j].Path
		}
		if strings.Join(paths, "\n") != strings.Join(want.Options, "\n") {
			t.Errorf("%s %s: options %q, golden %q", g.ID, role, paths, want.Options)
			continue
		}
		closeAll(t, g.ID+" "+string(role)+" logits", s.logits[i], want.Logits)
		closeAll(t, g.ID+" "+string(role)+" probs", s.probs[i], want.Probs)
	}
	kindProbs := make([]float64, len(g.Kind.Classes))
	kindLogits := make([]float64, len(g.Kind.Classes))
	for c, k := range g.Kind.Classes {
		for j, have := range r.kind.classes {
			if have == k {
				kindProbs[c], kindLogits[c] = s.kindProbs[j], s.kindLogits[j]
			}
		}
	}
	closeAll(t, g.ID+" kind logits", kindLogits, g.Kind.Logits)
	closeAll(t, g.ID+" kind probs", kindProbs, g.Kind.Probs)

	p, _ := r.compose(s)
	for _, role := range universal.Roles {
		if got := p.Path(role); got != g.Mapping[string(role)] {
			t.Errorf("%s: served %s = %q, golden %q", g.ID, role, got, g.Mapping[string(role)])
		}
	}
	if string(p.Kind) != g.Mapping["kind"] {
		t.Errorf("%s: served kind = %q, golden %q", g.ID, p.Kind, g.Mapping["kind"])
	}
}

func closeAll(t *testing.T, what string, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d values, golden %d", what, len(got), len(want))
		return
	}
	for i := range got {
		if math.Abs(got[i]-want[i]) > goldenTolerance {
			t.Errorf("%s[%d] = %.12g, golden %.12g", what, i, got[i], want[i])
		}
	}
}
