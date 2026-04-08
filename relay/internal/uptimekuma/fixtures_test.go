package uptimekuma

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixturesUnmarshal(t *testing.T) {
	files, err := os.ReadDir("../../testdata/uptimekuma")
	if err != nil {
		t.Fatalf("reading testdata/uptimekuma: %v", err)
	}

	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../testdata/uptimekuma", f.Name()))
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}

			var payload uptimekumaPayload
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Errorf("unmarshal uptimekumaPayload: %v", err)
			}
			if payload.Monitor.ID <= 0 {
				t.Error("expected positive monitor ID")
			}
		})
	}
}
