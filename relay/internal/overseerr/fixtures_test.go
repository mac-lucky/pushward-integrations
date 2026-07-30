package overseerr

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

const fixtureDir = "../../testdata/overseerr"

func fixtureNames(t *testing.T) []string {
	t.Helper()
	files, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("reading testdata/overseerr: %v", err)
	}
	var names []string
	for _, f := range files {
		if !f.IsDir() && filepath.Ext(f.Name()) == ".json" {
			names = append(names, f.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("testdata/overseerr has no fixtures")
	}
	return names
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, name)) // #nosec G304 -- fixture path is this package's own testdata dir, not user input
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	return data
}

func TestFixturesUnmarshal(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			var p overseerrPayload
			if err := json.Unmarshal(readFixture(t, name), &p); err != nil {
				t.Errorf("unmarshal overseerrPayload: %v", err)
			}
			if p.NotificationType == "" {
				t.Error("expected non-empty notification_type")
			}
		})
	}
}

// TestFixturesAccepted drives every fixture through the real handler. Decoding a
// fixture is not enough on its own: it does not prove the resulting content
// passes the server's per-template validation, which the mock enforces, and a
// fixture no handler ever sees cannot catch a regression.
func TestFixturesAccepted(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			h, _, _ := newHandler(t, testConfig())
			if w := send(t, h, string(readFixture(t, name))); w.Code != http.StatusOK {
				t.Errorf("expected 200, got %d (%s)", w.Code, w.Body.String())
			}
		})
	}
}
