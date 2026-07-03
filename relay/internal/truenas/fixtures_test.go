package truenas

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixturesUnmarshal(t *testing.T) {
	dir := "../../testdata/truenas"
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, f.Name()))
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}
			var p createAlert
			if err := json.Unmarshal(data, &p); err != nil {
				t.Errorf("unmarshal createAlert: %v", err)
			}
			if p.Alias == "" {
				t.Error("expected a non-empty alias")
			}
			if p.Message == "" {
				t.Error("expected a non-empty message")
			}
		})
	}
}
