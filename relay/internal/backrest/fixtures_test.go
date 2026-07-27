package backrest

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

const fixtureDir = "../../testdata/backrest"

func fixtureNames(t *testing.T) []string {
	t.Helper()
	files, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("reading %s: %v", fixtureDir, err)
	}
	var out []string
	for _, f := range files {
		if !f.IsDir() && filepath.Ext(f.Name()) == ".json" {
			out = append(out, f.Name())
		}
	}
	if len(out) == 0 {
		t.Fatalf("no fixtures found in %s", fixtureDir)
	}
	return out
}

func TestFixturesUnmarshal(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(fixtureDir, name)) // #nosec G304 -- fixture path is this package's own testdata dir, not user input
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}

			var p backrestPayload
			if err := json.Unmarshal(data, &p); err != nil {
				t.Errorf("unmarshal backrestPayload: %v", err)
			}
			if p.Event == "" {
				t.Error("expected non-empty event")
			}
			if _, ok := specFor(&p); !ok {
				t.Errorf("fixture uses %s, which the handler does not map", p.Event)
			}
		})
	}
}

// TestFixturesAccepted drives every fixture through the real handler. Decoding
// a fixture is not enough on its own: it does not prove the resulting content
// passes the server's per-template validation, which the mock enforces.
func TestFixturesAccepted(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(fixtureDir, name)) // #nosec G304 -- fixture path is this package's own testdata dir, not user input
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}
			h, ender, _, _ := newHandlerWithEnder(t, testConfig())
			// Drop the pending end timers before the mock server closes under
			// them, or they fire at a dead listener and log through the rest of
			// the package run.
			t.Cleanup(ender.StopAll)
			if w := send(t, h, string(data)); w.Code != http.StatusOK {
				t.Errorf("expected 200, got %d (%s)", w.Code, w.Body.String())
			}
		})
	}
}
