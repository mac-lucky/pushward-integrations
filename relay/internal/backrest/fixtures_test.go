package backrest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixturesUnmarshal(t *testing.T) {
	files, err := os.ReadDir("../../testdata/backrest")
	if err != nil {
		t.Fatalf("reading testdata/backrest: %v", err)
	}

	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../testdata/backrest", f.Name()))
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}

			var p webhookPayload
			if err := json.Unmarshal(data, &p); err != nil {
				t.Errorf("unmarshal webhookPayload: %v", err)
			}
			if p.Event == "" {
				t.Error("expected non-empty event")
			}
		})
	}
}
