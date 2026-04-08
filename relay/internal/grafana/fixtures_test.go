package grafana

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixturesUnmarshal(t *testing.T) {
	files, err := os.ReadDir("../../testdata/grafana")
	if err != nil {
		t.Fatalf("reading testdata/grafana: %v", err)
	}

	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../testdata/grafana", f.Name()))
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}
			var payload grafanaPayload
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Errorf("unmarshal into grafanaPayload: %v", err)
			}
			if len(payload.Alerts) == 0 {
				t.Error("expected at least one alert")
			}
		})
	}
}
