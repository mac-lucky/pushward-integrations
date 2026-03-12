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
			var envelope WebhookPayload
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

			var envelope WebhookPayload
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
			default:
				t.Errorf("unknown eventType %q", envelope.EventType)
			}
		})
	}
}
