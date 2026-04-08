package overseerr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixturesUnmarshal(t *testing.T) {
	files, err := os.ReadDir("../../testdata/overseerr")
	if err != nil {
		t.Fatalf("reading testdata/overseerr: %v", err)
	}

	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../testdata/overseerr", f.Name()))
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}

			var p overseerrPayload
			if err := json.Unmarshal(data, &p); err != nil {
				t.Errorf("unmarshal overseerrPayload: %v", err)
			}
			if p.NotificationType == "" {
				t.Error("expected non-empty notification_type")
			}
		})
	}
}
