package gitea

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGiteaFixturesUnmarshal(t *testing.T) {
	dir := "../../testdata/gitea"
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
			var p giteaPayload
			if err := json.Unmarshal(data, &p); err != nil {
				t.Errorf("unmarshal giteaPayload: %v", err)
			}
			if p.Repository == nil || p.Repository.FullName == "" {
				t.Error("expected a repository full_name")
			}
		})
	}
}

func TestForgejoFixturesUnmarshal(t *testing.T) {
	dir := "../../testdata/forgejo"
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
			var p forgejoPayload
			if err := json.Unmarshal(data, &p); err != nil {
				t.Errorf("unmarshal forgejoPayload: %v", err)
			}
			if p.Run == nil || p.Run.Repository == nil || p.Run.Repository.FullName == "" {
				t.Error("expected a run repository full_name")
			}
		})
	}
}
