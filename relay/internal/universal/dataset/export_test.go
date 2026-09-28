//go:build fixtureexport

package dataset

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/relaytest"
)

type record struct {
	Provider string           `json:"provider"`
	Route    string           `json:"route"`
	Fixture  string           `json:"fixture"`
	Primed   string           `json:"primed,omitempty"`
	Status   int              `json:"status"`
	Response string           `json:"response,omitempty"`
	Payload  json.RawMessage  `json:"payload"`
	Calls    []relaytest.Call `json:"calls"`
}

// TestExportFixtures sends each fixture to a fresh handler, as the providers'
// TestFixturesAccepted tests do, and writes what came in and what went out.
func TestExportFixtures(t *testing.T) {
	out := os.Getenv("UNIVERSAL_EXPORT_OUT")
	if out == "" {
		t.Skip("UNIVERSAL_EXPORT_OUT not set")
	}

	f, err := os.Create(out) // #nosec G304 G703 -- path chosen by whoever runs the export
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	// testdata/universal feeds the universal route's own tests. The export is
	// what the dedicated handlers make of their fixtures, so it stays out.
	seen := map[string]bool{"universal": true}
	total := 0
	for _, rt := range relaytest.Routes() {
		seen[rt.Dir] = true
		names, err := filepath.Glob(filepath.Join(relaytest.Testdata, rt.Dir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(names)
		for _, name := range names {
			t.Run(rt.Dir+"/"+filepath.Base(name), func(t *testing.T) {
				rec := exportOne(t, rt, name)
				if err := enc.Encode(rec); err != nil {
					t.Fatal(err)
				}
				total++
			})
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	// A fixture directory no route claims would silently drop out of the set.
	dirs, err := os.ReadDir(relaytest.Testdata)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if d.IsDir() && !seen[d.Name()] {
			t.Errorf("testdata/%s has no route in the exporter", d.Name())
		}
	}
	t.Logf("exported %d fixtures to %s", total, out)
}

func exportOne(t *testing.T, rt relaytest.Route, name string) record {
	t.Helper()
	res := relaytest.RunDedicated(t, rt, name)
	if res.Status != http.StatusOK {
		t.Errorf("POST %s: %d %s", rt.Path, res.Status, res.Response)
	}
	rec := record{
		Provider: rt.Provider,
		Route:    rt.Path,
		Fixture:  res.Fixture,
		Primed:   res.Primed,
		Status:   res.Status,
		Payload:  json.RawMessage(relaytest.Compact(t, res.Payload)),
		Calls:    relaytest.Calls(res.Calls),
	}
	if res.Status != http.StatusOK {
		rec.Response = res.Response
	}
	return rec
}
