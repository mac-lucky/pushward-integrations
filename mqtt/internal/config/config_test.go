package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	yml := `
mqtt:
  broker: "tcp://localhost:1883"
pushward:
  url: "http://localhost:8080"
  api_key: "hlk_test"
rules:
  - name: "Test"
    topic: "test/topic"
    slug: "test"
    lifecycle: "field"
    state_field: "status"
    state_map:
      on: pushward.StateOngoing
      off: pushward.StateEnded
`
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.MQTT.ClientID != "pushward-mqtt" {
		t.Errorf("expected default client_id, got %q", cfg.MQTT.ClientID)
	}
	if cfg.Polling.UpdateInterval.Seconds() != 5 {
		t.Errorf("expected 5s update interval, got %v", cfg.Polling.UpdateInterval)
	}
	if cfg.Rules[0].Template != "generic" {
		t.Errorf("expected default template 'generic', got %q", cfg.Rules[0].Template)
	}
}

func TestValidation_MissingBroker(t *testing.T) {
	yml := `
pushward:
  url: "http://localhost:8080"
  api_key: "hlk_test"
rules:
  - name: "Test"
    topic: "test/topic"
    slug: "test"
    lifecycle: "field"
    state_field: "status"
    state_map:
      on: pushward.StateOngoing
`
	path := filepath.Join(t.TempDir(), "config.yml")
	os.WriteFile(path, []byte(yml), 0o644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing broker")
	}
}

func TestValidation_NoRules(t *testing.T) {
	yml := `
mqtt:
  broker: "tcp://localhost:1883"
pushward:
  url: "http://localhost:8080"
  api_key: "hlk_test"
rules: []
`
	path := filepath.Join(t.TempDir(), "config.yml")
	os.WriteFile(path, []byte(yml), 0o644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for no rules")
	}
}

func TestValidation_FieldLifecycle_MissingStateField(t *testing.T) {
	yml := `
mqtt:
  broker: "tcp://localhost:1883"
pushward:
  url: "http://localhost:8080"
  api_key: "hlk_test"
rules:
  - name: "Test"
    topic: "test/topic"
    slug: "test"
    lifecycle: "field"
`
	path := filepath.Join(t.TempDir(), "config.yml")
	os.WriteFile(path, []byte(yml), 0o644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing state_field")
	}
}

func TestValidation_PresenceLifecycle_MissingTimeout(t *testing.T) {
	yml := `
mqtt:
  broker: "tcp://localhost:1883"
pushward:
  url: "http://localhost:8080"
  api_key: "hlk_test"
rules:
  - name: "Test"
    topic: "test/topic"
    slug: "test"
    lifecycle: "presence"
`
	path := filepath.Join(t.TempDir(), "config.yml")
	os.WriteFile(path, []byte(yml), 0o644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing inactivity_timeout")
	}
}
