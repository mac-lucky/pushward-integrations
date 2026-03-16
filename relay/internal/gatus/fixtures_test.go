package gatus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixturesUnmarshal(t *testing.T) {
	files, err := os.ReadDir("../../testdata/gatus")
	if err != nil {
		t.Fatalf("reading testdata/gatus: %v", err)
	}

	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../testdata/gatus", f.Name()))
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}

			var payload webhookPayload
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Errorf("unmarshal webhookPayload: %v", err)
			}
			if payload.EndpointName == "" {
				t.Error("expected non-empty endpoint_name")
			}
		})
	}
}
