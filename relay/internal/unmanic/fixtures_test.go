package unmanic

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixturesUnmarshal(t *testing.T) {
	files, err := os.ReadDir("../../testdata/unmanic")
	if err != nil {
		t.Fatalf("reading testdata/unmanic: %v", err)
	}

	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../testdata/unmanic", f.Name()))
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}

			var p apprisePayload
			if err := json.Unmarshal(data, &p); err != nil {
				t.Errorf("unmarshal apprisePayload: %v", err)
			}
			if p.Type == "" {
				t.Error("expected non-empty type")
			}
		})
	}
}
