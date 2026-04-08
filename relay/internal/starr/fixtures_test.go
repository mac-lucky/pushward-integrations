package starr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixturesUnmarshal_Radarr(t *testing.T) {
	files, err := os.ReadDir("../../testdata/radarr")
	if err != nil {
		t.Fatalf("reading testdata/radarr: %v", err)
	}

	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../testdata/radarr", f.Name()))
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}

			// Determine type from eventType field.
			var envelope starrPayload
			if err := json.Unmarshal(data, &envelope); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}

			switch envelope.EventType {
			case "Grab":
				var p RadarrGrabPayload
				if err := json.Unmarshal(data, &p); err != nil {
					t.Errorf("unmarshal RadarrGrabPayload: %v", err)
				}
			case "Download":
				var p RadarrDownloadPayload
				if err := json.Unmarshal(data, &p); err != nil {
					t.Errorf("unmarshal RadarrDownloadPayload: %v", err)
				}
			case "Test":
				// Test events are valid, no specific payload to unmarshal
			case "Health":
				var p HealthPayload
				if err := json.Unmarshal(data, &p); err != nil {
					t.Errorf("unmarshal HealthPayload: %v", err)
				}
				if p.Message == "" {
					t.Error("expected non-empty message")
				}
			case "HealthRestored":
				var p HealthRestoredPayload
				if err := json.Unmarshal(data, &p); err != nil {
					t.Errorf("unmarshal HealthRestoredPayload: %v", err)
				}
				if p.Message == "" {
					t.Error("expected non-empty message")
				}
			case "ManualInteractionRequired":
				var p ManualInteractionPayload
				if err := json.Unmarshal(data, &p); err != nil {
					t.Errorf("unmarshal ManualInteractionPayload: %v", err)
				}
				if p.DownloadID == "" {
					t.Error("expected non-empty downloadId")
				}
			default:
				t.Errorf("unknown eventType %q", envelope.EventType)
			}
		})
	}
}

func TestFixturesUnmarshal_Sonarr(t *testing.T) {
	files, err := os.ReadDir("../../testdata/sonarr")
	if err != nil {
		t.Fatalf("reading testdata/sonarr: %v", err)
	}

	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../testdata/sonarr", f.Name()))
			if err != nil {
				t.Fatalf("reading file: %v", err)
			}

			var envelope starrPayload
			if err := json.Unmarshal(data, &envelope); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}

			switch envelope.EventType {
			case "Grab":
				var p SonarrGrabPayload
				if err := json.Unmarshal(data, &p); err != nil {
					t.Errorf("unmarshal SonarrGrabPayload: %v", err)
				}
			case "Download":
				var p SonarrDownloadPayload
				if err := json.Unmarshal(data, &p); err != nil {
					t.Errorf("unmarshal SonarrDownloadPayload: %v", err)
				}
			case "Test":
				// Test events are valid, no specific payload to unmarshal
			case "Health":
				var p HealthPayload
				if err := json.Unmarshal(data, &p); err != nil {
					t.Errorf("unmarshal HealthPayload: %v", err)
				}
				if p.Message == "" {
					t.Error("expected non-empty message")
				}
			case "HealthRestored":
				var p HealthRestoredPayload
				if err := json.Unmarshal(data, &p); err != nil {
					t.Errorf("unmarshal HealthRestoredPayload: %v", err)
				}
				if p.Message == "" {
					t.Error("expected non-empty message")
				}
			case "ManualInteractionRequired":
				var p ManualInteractionPayload
				if err := json.Unmarshal(data, &p); err != nil {
					t.Errorf("unmarshal ManualInteractionPayload: %v", err)
				}
				if p.DownloadID == "" {
					t.Error("expected non-empty downloadId")
				}
			default:
				t.Errorf("unknown eventType %q", envelope.EventType)
			}
		})
	}
}
