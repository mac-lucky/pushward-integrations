package changedetection

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixturesUnmarshal(t *testing.T) {
	files, err := os.ReadDir("../../testdata/changedetection")
	if err != nil {
		t.Fatalf("reading testdata/changedetection: %v", err)
	}

	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../testdata/changedetection", f.Name()))
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}
			var payload changedetectionPayload
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Errorf("unmarshal into changedetectionPayload: %v", err)
			}
			if payload.URL == "" {
				t.Error("expected non-empty url")
			}
		})
	}
}
