package bazarr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/apprise"
)

func TestFixturesUnmarshal(t *testing.T) {
	files, err := os.ReadDir("../../testdata/bazarr")
	if err != nil {
		t.Fatalf("reading testdata/bazarr: %v", err)
	}

	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../testdata/bazarr", f.Name()))
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}

			var p apprise.Payload
			if err := json.Unmarshal(data, &p); err != nil {
				t.Errorf("unmarshal apprisePayload: %v", err)
			}
			if p.Message == "" {
				t.Error("expected non-empty message")
			}
		})
	}
}
