package komodo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixturesUnmarshal(t *testing.T) {
	dir := "../../testdata/komodo"
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
			var p komodoPayload
			if err := json.Unmarshal(data, &p); err != nil {
				t.Errorf("unmarshal komodoPayload: %v", err)
			}
			if p.Data.Type == "" {
				t.Error("expected a non-empty data.type")
			}
		})
	}
}

// TestTolerantIDDecode pins that _id decodes whether it is a Mongo $oid object,
// a plain string, or absent, since the bridge never reads it.
func TestTolerantIDDecode(t *testing.T) {
	cases := map[string]string{
		"oid object":   `{"_id":{"$oid":"6650f1c2a1b2c3d4e5f60789"},"ts":1,"level":"OK","target":{"type":"System","id":"s"},"data":{"type":"Test","data":{}}}`,
		"plain string": `{"_id":"abc","ts":1,"level":"OK","target":{"type":"System","id":"s"},"data":{"type":"Test","data":{}}}`,
		"absent":       `{"ts":1,"level":"OK","target":{"type":"System","id":"s"},"data":{"type":"Test","data":{}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var p komodoPayload
			if err := json.Unmarshal([]byte(body), &p); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if p.Data.Type != "Test" {
				t.Errorf("expected Test, got %s", p.Data.Type)
			}
		})
	}
}
