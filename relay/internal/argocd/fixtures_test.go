package argocd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixturesUnmarshal(t *testing.T) {
	files, err := os.ReadDir("../../testdata/argocd")
	if err != nil {
		t.Fatalf("reading testdata/argocd: %v", err)
	}

	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../testdata/argocd", f.Name()))
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}
			var payload webhookPayload
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Errorf("unmarshal into webhookPayload: %v", err)
			}
			if payload.App == "" {
				t.Error("expected non-empty app field")
			}
			if payload.Event == "" {
				t.Error("expected non-empty event field")
			}
		})
	}
}
