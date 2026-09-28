package ranker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
)

func embedded(t testing.TB) *Ranker {
	t.Helper()
	r, err := load()
	if err != nil {
		t.Fatalf("embedded weights: %v", err)
	}
	return r
}

// withServe is r with its serve flags replaced: every role and the kind set
// to on.
func withServe(r *Ranker, on bool) *Ranker {
	c := *r
	c.serve = make([]bool, len(universal.Roles))
	for i := range c.serve {
		c.serve[i] = on
	}
	c.serveKind = on
	return &c
}

func propose(t testing.TB, r *Ranker, shapes []universal.ShapeField) universal.Result {
	t.Helper()
	res, err := r.Propose(context.Background(), universal.NewInput("test", nil, shapes, false))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestEmbeddedWeights(t *testing.T) {
	r := embedded(t)
	id, pass, err := Info()
	if err != nil || id != r.id || pass != r.pass {
		t.Fatalf("Info() = %q, %v, %v; loaded %q, %v", id, pass, err, r.id, r.pass)
	}
	p, ok := New()
	if ok != r.pass || (p != nil) != ok {
		t.Errorf("New() = %v, %v with gate pass %v", p, ok, r.pass)
	}
	if !json.Valid(weightsJSON) || strings.ContainsFunc(string(weightsJSON), func(c rune) bool { return c > 127 }) {
		t.Error("weights.json must be valid JSON in ASCII")
	}
}

// minimalWeights is a weights file that loads, for the parse tests to break.
func minimalWeights() map[string]any {
	roles := map[string]any{}
	for _, r := range universal.Roles {
		roles[string(r)] = map[string]any{"intercept": 0.0, "temperature": 1.0, "w": map[string]any{"k:title": 1.0}}
	}
	return map[string]any{
		"schema": WeightsSchema, "features_version": FeaturesVersion, "top_k": TopK, "id": "test",
		"gate":  map[string]any{"rule": "v2", "pass": true},
		"serve": map[string]any{"title": true, "kind": false},
		"roles": roles,
		"kind": map[string]any{
			"classes": []string{"alert", "notification", "progress"}, "intercept": []float64{0, 0, 0},
			"temperature": 1.0, "w": map[string]any{"hk:alert": []float64{1, 0, 0}},
		},
	}
}

func parseMap(t *testing.T, m map[string]any) (*Ranker, error) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return parse(b)
}

func TestParse(t *testing.T) {
	r, err := parseMap(t, minimalWeights())
	if err != nil {
		t.Fatal(err)
	}
	if !r.pass || !r.serve[0] || r.serveKind || r.by != "ranker/test" {
		t.Errorf("parsed %+v", r)
	}
	if p, ok := proposer(r, nil); !ok || p == nil {
		t.Error("weights that passed their gate must propose")
	}
	m := minimalWeights()
	m["gate"] = map[string]any{"rule": "v2", "pass": false}
	r, err = parseMap(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := proposer(r, nil); ok || p != nil {
		t.Error("weights that failed their gate must not propose")
	}
	if p, ok := proposer(nil, fmt.Errorf("broken")); ok || p != nil {
		t.Error("weights that do not load must not propose")
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"schema":           func(m map[string]any) { m["schema"] = WeightsSchema + 1 },
		"features version": func(m map[string]any) { m["features_version"] = FeaturesVersion + 1 },
		"top k":            func(m map[string]any) { m["top_k"] = TopK - 1 },
		"no id":            func(m map[string]any) { m["id"] = "" },
		"missing role":     func(m map[string]any) { delete(m["roles"].(map[string]any), "lifecycle") },
		"unknown role": func(m map[string]any) {
			m["roles"].(map[string]any)["image"] = map[string]any{"temperature": 1.0}
		},
		"zero temperature": func(m map[string]any) {
			m["roles"].(map[string]any)["title"] = map[string]any{"temperature": 0.0}
		},
		"unknown serve key": func(m map[string]any) { m["serve"] = map[string]any{"image": true} },
		"kind classes": func(m map[string]any) {
			m["kind"].(map[string]any)["classes"] = []string{"alert", "alert", "progress"}
		},
		"kind intercepts": func(m map[string]any) { m["kind"].(map[string]any)["intercept"] = []float64{0} },
		"kind weight width": func(m map[string]any) {
			m["kind"].(map[string]any)["w"] = map[string]any{"hk:alert": []float64{1}}
		},
		"kind temperature": func(m map[string]any) { m["kind"].(map[string]any)["temperature"] = -1.0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := minimalWeights()
			mutate(m)
			if _, err := parseMap(t, m); err == nil {
				t.Error("parse accepted it")
			}
		})
	}
	if _, err := parse([]byte("{")); err == nil {
		t.Error("parse accepted broken JSON")
	}
}

// With nothing served the ranker is the heuristic, value tables and kind
// included, on every relay fixture and edge case.
func TestNothingServedIsTheHeuristic(t *testing.T) {
	r := withServe(embedded(t), false)
	for name, body := range testPayloads(t) {
		shapes := shapesOf(t, string(body))
		res := propose(t, r, shapes)
		if want := universal.ProposeShapes(shapes); !reflect.DeepEqual(res.Proposal, want) {
			t.Errorf("%s: proposal =\n%+v\nheuristic =\n%+v", name, res.Proposal, want)
		}
		for role, s := range res.Scores.Roles {
			if s.By != byHeuristic {
				t.Errorf("%s: %s decided by %q", name, role, s.By)
			}
		}
	}
}

// Whatever it serves, the ranker proposes mappings the relay accepts: every
// path mappable to its role, title and body apart, a field in one role only
// (correlation and url excepted), and value tables that match the fields.
func TestProposalsValidate(t *testing.T) {
	base := embedded(t)
	for _, r := range []*Ranker{base, withServe(base, true)} {
		for name, body := range testPayloads(t) {
			fields, truncated, err := universal.Flatten(strings.NewReader(string(body)))
			if err != nil {
				t.Fatal(err)
			}
			shapes := universal.ShapesOf(fields)
			p := propose(t, r, shapes).Proposal
			m := universal.NewMapping(p, shapes)
			if err := m.Validate(universal.NewShape(shapes, truncated)); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			seen := map[string]universal.Role{}
			for _, role := range universal.Roles {
				path := p.Path(role)
				if path == "" || universal.Reusable(role) {
					continue
				}
				if other, dup := seen[path]; dup {
					t.Errorf("%s: %s is both %s and %s", name, path, other, role)
				}
				seen[path] = role
			}
			tables := p
			tables.SetTables(shapes)
			if !reflect.DeepEqual(tables, p) {
				t.Errorf("%s: value tables do not match the proposed fields", name)
			}
		}
	}
}

func TestScores(t *testing.T) {
	r := embedded(t)
	shapes := shapesOf(t, `{"alert": {"title": "Disk almost full", "description": "Only 3% left on /var", "status": "firing",
		"severity": "critical", "id": "a1b2c3d4"}, "url": "https://example.com/alerts/1"}`)
	res := propose(t, r, shapes)
	if res.By != "ranker/"+r.id || res.Scores == nil || res.Scores.Weights != r.id {
		t.Fatalf("result = %+v", res)
	}
	for i, role := range universal.Roles {
		s, ok := res.Scores.Roles[role]
		if !ok {
			t.Fatalf("no score for %s", role)
		}
		if s.Path != res.Proposal.Path(role) {
			t.Errorf("%s: score path %q, proposal %q", role, s.Path, res.Proposal.Path(role))
		}
		want := byHeuristic
		if r.serve[i] {
			want = byRanker
		}
		if s.By != want {
			t.Errorf("%s: by %q, want %q", role, s.By, want)
		}
		if len(s.Top3) == 0 || len(s.Top3) > 3 || !slices.IsSortedFunc(s.Top3, func(a, b universal.Candidate) int {
			return -cmpFloat(a.Score, b.Score)
		}) {
			t.Errorf("%s: top3 = %+v", role, s.Top3)
		}
		if s.P < 0 || s.P > 1 {
			t.Errorf("%s: p = %v", role, s.P)
		}
	}
	sum := 0.0
	for _, p := range res.Scores.Kind {
		sum += p
	}
	if len(res.Scores.Kind) != 3 || math.Abs(sum-1) > 1e-9 {
		t.Errorf("kind probabilities = %v", res.Scores.Kind)
	}
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func TestProposeCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := embedded(t).Propose(ctx, universal.NewInput("test", nil, nil, false)); err == nil {
		t.Error("a canceled context must fail the proposal")
	}
}

func TestEmptyPayload(t *testing.T) {
	res := propose(t, embedded(t), nil)
	for _, role := range universal.Roles {
		if p := res.Proposal.Path(role); p != "" {
			t.Errorf("%s = %q on an empty payload", role, p)
		}
	}
}

// testPayloads is every relay fixture plus a few shapes the fixtures lack.
func testPayloads(t testing.TB) map[string][]byte {
	t.Helper()
	out := relayFixtures(t)
	for name, body := range map[string]string{
		"edge/unicode": "{\"t\u00edtulo\": \"Falha no backup\", \"estado\": \"firing\", \"\u6570\u636e\": {\"\u540d\u79f0\": \"x\"}}",
		"edge/array":   `[{"title": "First", "status": "queued", "progress": 0.25}]`,
		"edge/empty":   `{"title": "", "message": "", "status": ""}`,
		"edge/secrets": `{"token": "` + fakeToken + `", "title": "Deploy", "email": "a@b.example"}`,
		"edge/wide":    wide(300),
	} {
		out[name] = []byte(body)
	}
	return out
}

// fakeToken looks like a GitHub token to the redaction rules.
const fakeToken = "ghp_" + "a1B2c3D4e5F6g7H8i9J0k"

// wide is a flat payload of n text fields with a title and a status last.
func wide(n int) string {
	var b strings.Builder
	b.WriteString("{")
	for i := range n {
		fmt.Fprintf(&b, `"field_%03d": "value %d", `, i, i)
	}
	b.WriteString(`"title": "Past the cap", "status": "resolved"}`)
	return b.String()
}

// A realistic 50-field alert: labels, annotations, links, timestamps and ids.
func fiftyFields() string {
	var b strings.Builder
	b.WriteString(`{"receiver": "pushward", "status": "firing", "externalURL": "https://am.example.com",
		"groupKey": "{}:{alertname=\"HighCPU\"}", "truncatedAlerts": 0, "version": "4",
		"commonLabels": {"alertname": "HighCPU", "severity": "warning", "cluster": "prod"},
		"alerts": [{"status": "firing", "startsAt": "2026-09-28T10:00:00Z", "fingerprint": "3f2b8c1e9d0a",
			"generatorURL": "https://prom.example.com/graph?g0.expr=up",
			"labels": {"alertname": "HighCPU", "instance": "node-1:9100", "job": "node", "severity": "warning"},
			"annotations": {"summary": "CPU above 90%", "description": "node-1 has been above 90% CPU for 10 minutes",
				"runbook_url": "https://runbooks.example.com/cpu"}}]`)
	for i := range 15 {
		fmt.Fprintf(&b, `, "extra_%02d": {"name": "item %d", "count": %d}`, i, i, i)
	}
	b.WriteString("}")
	return b.String()
}

// BenchmarkPropose50 is the ranker's own cost on a 50-field payload: the
// universal handler computes the shapes before it asks a proposer.
func BenchmarkPropose50(b *testing.B) {
	benchmarkPropose(b, true)
}

// BenchmarkPropose50Shapes adds computing the shapes.
func BenchmarkPropose50Shapes(b *testing.B) {
	benchmarkPropose(b, false)
}

func benchmarkPropose(b *testing.B, shaped bool) {
	fields, _, err := universal.Flatten(strings.NewReader(fiftyFields()))
	if err != nil {
		b.Fatal(err)
	}
	if len(fields) != 50 {
		b.Fatalf("%d fields, want 50", len(fields))
	}
	var shapes []universal.ShapeField
	if shaped {
		shapes = universal.ShapesOf(fields)
	}
	r := embedded(b)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := r.Propose(ctx, universal.NewInput("bench", fields, shapes, false)); err != nil {
			b.Fatal(err)
		}
	}
}
