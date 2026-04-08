package paperless

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixturesUnmarshal(t *testing.T) {
	files, err := os.ReadDir("../../testdata/paperless")
	if err != nil {
		t.Fatalf("reading testdata/paperless: %v", err)
	}

	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../testdata/paperless", f.Name()))
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}

			var p paperlessPayload
			if err := json.Unmarshal(data, &p); err != nil {
				t.Errorf("unmarshal paperlessPayload: %v", err)
			}
			if p.Event == "" {
				t.Error("expected non-empty event")
			}
		})
	}
}
