//go:build fixtureexport

package dataset

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universal/ranker"
)

// topK is how many ranked candidates per role go out with each proposal, 8
// unless UNIVERSAL_TOPK says otherwise. A model choosing between options gets
// these as its choices.
func topK(t *testing.T) int {
	s := os.Getenv("UNIVERSAL_TOPK")
	if s == "" {
		return 8
	}
	k, err := strconv.Atoi(s)
	if err != nil || k < 1 {
		t.Fatalf("UNIVERSAL_TOPK=%q: want a positive integer", s)
	}
	return k
}

// proposeIn carries a payload, or a stored shape in its place: either the
// Shape object or a bare list of ShapeField.
type proposeIn struct {
	ID      string          `json:"id"`
	Source  string          `json:"source"`
	Payload json.RawMessage `json:"payload"`
	Shape   json.RawMessage `json:"shape"`
}

type proposeOut struct {
	ID          string                                   `json:"id"`
	Fingerprint string                                   `json:"fingerprint"`
	Truncated   bool                                     `json:"truncated,omitempty"`
	Error       string                                   `json:"error,omitempty"`
	Fields      []fieldOut                               `json:"fields"`
	Proposal    universal.Proposal                       `json:"proposal"`
	Candidates  map[universal.Role][]universal.Candidate `json:"candidates"`
	Micros      float64                                  `json:"micros"`

	// With UNIVERSAL_FEATURES=1: the ranker's options per role (its top
	// ranker.TopK candidates plus none, each with its features), the kind
	// model's features, and the heuristic's minimum score per role.
	Options      map[universal.Role][]ranker.Option `json:"options,omitempty"`
	KindFeatures ranker.Features                    `json:"kind_features,omitempty"`
	MinScore     map[universal.Role]float64         `json:"min_score,omitempty"`
}

// fieldOut is a flattened field plus what the heuristic sees of it. Value is
// empty for a record that came in as a shape.
type fieldOut struct {
	universal.Field
	Shape  universal.ShapeField `json:"shape"`
	Tokens universal.PathTokens `json:"tokens"`
}

// TestProposeJSONL reads {id, source, payload} lines, or {id, source, shape}
// ones, and writes the flattened fields, the heuristic's proposal and its top
// candidates for every role.
func TestProposeJSONL(t *testing.T) {
	in, out := os.Getenv("UNIVERSAL_PROPOSE_IN"), os.Getenv("UNIVERSAL_PROPOSE_OUT")
	if in == "" || out == "" {
		t.Skip("UNIVERSAL_PROPOSE_IN / UNIVERSAL_PROPOSE_OUT not set")
	}
	k := topK(t)
	feats := os.Getenv("UNIVERSAL_FEATURES") == "1"
	src, err := os.Open(in) // #nosec G304 G703 -- path chosen by whoever runs the export
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.Create(out) // #nosec G304 G703 -- path chosen by whoever runs the export
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dst.Close() }()

	w := bufio.NewWriter(dst)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	n := 0
	for sc.Scan() {
		var rec proposeIn
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("line %d: %v", n+1, err)
		}
		if err := enc.Encode(propose(rec, k, feats)); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	t.Logf("proposed %d payloads to %s", n, out)
}

func propose(rec proposeIn, k int, feats bool) proposeOut {
	res := proposeOut{ID: rec.ID, Candidates: map[universal.Role][]universal.Candidate{}}
	start := time.Now()
	var (
		fields    []universal.Field
		shapes    []universal.ShapeField
		truncated bool
		err       error
	)
	if len(rec.Payload) == 0 && len(rec.Shape) > 0 {
		shapes, truncated, err = parseShape(rec.Shape)
		fields = make([]universal.Field, len(shapes))
		for i, s := range shapes {
			fields[i] = universal.Field{Path: s.Path, Type: s.Type}
		}
	} else {
		fields, truncated, err = universal.Flatten(bytes.NewReader(rec.Payload))
		shapes = universal.ShapesOf(fields)
	}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Proposal = universal.ProposeShapes(shapes)
	res.Micros = float64(time.Since(start).Nanoseconds()) / 1e3
	res.Truncated = truncated
	res.Fingerprint = universal.Fingerprint(rec.Source, fields)
	res.Fields = make([]fieldOut, len(fields))
	for i, f := range fields {
		res.Fields[i] = fieldOut{Field: f, Shape: shapes[i], Tokens: universal.TokensOf(f.Path)}
	}
	for _, r := range universal.Roles {
		c := universal.RankShapes(shapes, r)
		res.Candidates[r] = c[:min(len(c), k)]
	}
	if feats {
		ex := ranker.Extract(shapes)
		res.Options = make(map[universal.Role][]ranker.Option, len(universal.Roles))
		res.MinScore = make(map[universal.Role]float64, len(universal.Roles))
		for i, r := range universal.Roles {
			res.Options[r] = ex.Options[i]
			res.MinScore[r] = universal.MinScore(r)
		}
		res.KindFeatures = ex.Kind
	}
	return res
}

func parseShape(raw json.RawMessage) ([]universal.ShapeField, bool, error) {
	if raw = bytes.TrimSpace(raw); len(raw) > 0 && raw[0] == '[' {
		var fields []universal.ShapeField
		err := json.Unmarshal(raw, &fields)
		return fields, false, err
	}
	var s universal.Shape
	err := json.Unmarshal(raw, &s)
	return s.Fields, s.Truncated, err
}
