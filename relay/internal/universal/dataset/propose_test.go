//go:build fixtureexport

package dataset

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
)

// topK is how many ranked candidates per role go out with each proposal. A
// model choosing between options gets these as its choices.
const topK = 8

type proposeIn struct {
	ID      string          `json:"id"`
	Source  string          `json:"source"`
	Payload json.RawMessage `json:"payload"`
}

type proposeOut struct {
	ID          string                                   `json:"id"`
	Fingerprint string                                   `json:"fingerprint"`
	Truncated   bool                                     `json:"truncated,omitempty"`
	Error       string                                   `json:"error,omitempty"`
	Fields      []universal.Field                        `json:"fields"`
	Proposal    universal.Proposal                       `json:"proposal"`
	Candidates  map[universal.Role][]universal.Candidate `json:"candidates"`
	Micros      float64                                  `json:"micros"`
}

// TestProposeJSONL reads {id, source, payload} lines and writes the flattened
// fields, the heuristic's proposal and its top candidates for every role.
func TestProposeJSONL(t *testing.T) {
	in, out := os.Getenv("UNIVERSAL_PROPOSE_IN"), os.Getenv("UNIVERSAL_PROPOSE_OUT")
	if in == "" || out == "" {
		t.Skip("UNIVERSAL_PROPOSE_IN / UNIVERSAL_PROPOSE_OUT not set")
	}
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
		if err := enc.Encode(propose(rec)); err != nil {
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

func propose(rec proposeIn) proposeOut {
	res := proposeOut{ID: rec.ID, Candidates: map[universal.Role][]universal.Candidate{}}
	start := time.Now()
	fields, truncated, err := universal.Flatten(bytes.NewReader(rec.Payload))
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Proposal = universal.Propose(fields)
	res.Micros = float64(time.Since(start).Nanoseconds()) / 1e3
	res.Fields, res.Truncated = fields, truncated
	res.Fingerprint = universal.Fingerprint(rec.Source, fields)
	for _, r := range universal.Roles {
		c := universal.Rank(fields, r)
		res.Candidates[r] = c[:min(len(c), topK)]
	}
	return res
}
