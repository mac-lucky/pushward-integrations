package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- LoadYAML ---

func TestLoadYAML_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	os.WriteFile(path, []byte("url: http://localhost:8080\napi_key: hlk_test\npriority: 3\n"), 0644)

	var cfg PushWardConfig
	if err := LoadYAML(path, &cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.URL != "http://localhost:8080" {
		t.Errorf("expected url 'http://localhost:8080', got %q", cfg.URL)
	}
	if cfg.APIKey != "hlk_test" {
		t.Errorf("expected api_key 'hlk_test', got %q", cfg.APIKey)
	}
	if cfg.Priority != 3 {
		t.Errorf("expected priority 3, got %d", cfg.Priority)
	}
}

func TestLoadYAML_MissingFile(t *testing.T) {
	var cfg PushWardConfig
	err := LoadYAML("/nonexistent/path/config.yml", &cfg)
	if err != nil {
		t.Fatalf("expected nil for missing file (tolerated), got %v", err)
	}
}

func TestLoadYAML_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yml")
	os.WriteFile(path, []byte(":::invalid yaml{{{"), 0644)

	var cfg PushWardConfig
	err := LoadYAML(path, &cfg)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoadYAML_PermissionError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "noperm.yml")
	os.WriteFile(path, []byte("url: test"), 0644)
	os.Chmod(path, 0000)
	t.Cleanup(func() { os.Chmod(path, 0644) })

	var cfg PushWardConfig
	err := LoadYAML(path, &cfg)
	if err == nil {
		t.Fatal("expected error for unreadable file")
	}
}

func TestLoadYAML_DurationFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	os.WriteFile(path, []byte("cleanup_delay: 15m\nstale_timeout: 30m\nend_delay: 5s\nend_display_time: 4s\n"), 0644)

	var cfg PushWardConfig
	if err := LoadYAML(path, &cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CleanupDelay != 15*time.Minute {
		t.Errorf("expected cleanup_delay 15m, got %v", cfg.CleanupDelay)
	}
	if cfg.StaleTimeout != 30*time.Minute {
		t.Errorf("expected stale_timeout 30m, got %v", cfg.StaleTimeout)
	}
	if cfg.EndDelay != 5*time.Second {
		t.Errorf("expected end_delay 5s, got %v", cfg.EndDelay)
	}
	if cfg.EndDisplayTime != 4*time.Second {
		t.Errorf("expected end_display_time 4s, got %v", cfg.EndDisplayTime)
	}
}

// --- ApplyEnvOverrides (PushWardConfig) ---

func TestApplyEnvOverrides_AllVars(t *testing.T) {
	t.Setenv("PUSHWARD_URL", "http://env-url:9090")
	t.Setenv("PUSHWARD_API_KEY", "hlk_env")
	t.Setenv("PUSHWARD_PRIORITY", "7")
	t.Setenv("PUSHWARD_CLEANUP_DELAY", "20m")
	t.Setenv("PUSHWARD_STALE_TIMEOUT", "1h")
	t.Setenv("PUSHWARD_END_DELAY", "3s")
	t.Setenv("PUSHWARD_END_DISPLAY_TIME", "6s")

	cfg := PushWardConfig{
		URL:      "http://original",
		APIKey:   "hlk_original",
		Priority: 1,
	}
	if err := cfg.ApplyEnvOverrides(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.URL != "http://env-url:9090" {
		t.Errorf("expected URL from env, got %q", cfg.URL)
	}
	if cfg.APIKey != "hlk_env" {
		t.Errorf("expected APIKey from env, got %q", cfg.APIKey)
	}
	if cfg.Priority != 7 {
		t.Errorf("expected Priority 7, got %d", cfg.Priority)
	}
	if cfg.CleanupDelay != 20*time.Minute {
		t.Errorf("expected CleanupDelay 20m, got %v", cfg.CleanupDelay)
	}
	if cfg.StaleTimeout != 1*time.Hour {
		t.Errorf("expected StaleTimeout 1h, got %v", cfg.StaleTimeout)
	}
	if cfg.EndDelay != 3*time.Second {
		t.Errorf("expected EndDelay 3s, got %v", cfg.EndDelay)
	}
	if cfg.EndDisplayTime != 6*time.Second {
		t.Errorf("expected EndDisplayTime 6s, got %v", cfg.EndDisplayTime)
	}
}

func TestApplyEnvOverrides_NoVars(t *testing.T) {
	cfg := PushWardConfig{
		URL:    "http://keep",
		APIKey: "hlk_keep",
	}
	if err := cfg.ApplyEnvOverrides(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.URL != "http://keep" {
		t.Errorf("expected URL unchanged, got %q", cfg.URL)
	}
	if cfg.APIKey != "hlk_keep" {
		t.Errorf("expected APIKey unchanged, got %q", cfg.APIKey)
	}
}

func TestApplyEnvOverrides_InvalidPriority(t *testing.T) {
	t.Setenv("PUSHWARD_PRIORITY", "not-a-number")
	cfg := PushWardConfig{}
	err := cfg.ApplyEnvOverrides()
	if err == nil {
		t.Fatal("expected error for invalid priority")
	}
}

func TestApplyEnvOverrides_InvalidDuration(t *testing.T) {
	tests := []struct {
		envVar string
		value  string
	}{
		{"PUSHWARD_CLEANUP_DELAY", "bad"},
		{"PUSHWARD_STALE_TIMEOUT", "xyz"},
		{"PUSHWARD_END_DELAY", "nope"},
		{"PUSHWARD_END_DISPLAY_TIME", "!!"},
	}
	for _, tt := range tests {
		t.Run(tt.envVar, func(t *testing.T) {
			t.Setenv(tt.envVar, tt.value)
			cfg := PushWardConfig{}
			if err := cfg.ApplyEnvOverrides(); err == nil {
				t.Errorf("expected error for invalid %s", tt.envVar)
			}
		})
	}
}

// --- ApplyEnvOverrides (ServerConfig) ---

func TestServerConfig_ApplyEnvOverrides(t *testing.T) {
	t.Setenv("PUSHWARD_SERVER_ADDRESS", ":9999")
	cfg := ServerConfig{Address: ":8090"}
	cfg.ApplyEnvOverrides()
	if cfg.Address != ":9999" {
		t.Errorf("expected ':9999', got %q", cfg.Address)
	}
}

func TestServerConfig_ApplyEnvOverrides_MetricsAddress(t *testing.T) {
	t.Setenv("PUSHWARD_SERVER_METRICS_ADDRESS", ":9191")
	cfg := ServerConfig{MetricsAddress: ":9090"}
	cfg.ApplyEnvOverrides()
	if cfg.MetricsAddress != ":9191" {
		t.Errorf("expected ':9191', got %q", cfg.MetricsAddress)
	}
}

func TestServerConfig_ApplyEnvOverrides_NoVar(t *testing.T) {
	cfg := ServerConfig{Address: ":8090", MetricsAddress: ":9090"}
	cfg.ApplyEnvOverrides()
	if cfg.Address != ":8090" {
		t.Errorf("expected ':8090' unchanged, got %q", cfg.Address)
	}
	if cfg.MetricsAddress != ":9090" {
		t.Errorf("expected ':9090' unchanged, got %q", cfg.MetricsAddress)
	}
}

// --- Validate ---

func TestValidate_Valid(t *testing.T) {
	cfg := PushWardConfig{URL: "http://localhost", APIKey: "hlk_test", Priority: 5}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_MissingURL(t *testing.T) {
	cfg := PushWardConfig{APIKey: "hlk_test"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for missing URL")
	}
}

func TestValidate_MissingAPIKey(t *testing.T) {
	cfg := PushWardConfig{URL: "http://localhost"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for missing API key")
	}
}

func TestValidate_PriorityTooLow(t *testing.T) {
	cfg := PushWardConfig{URL: "http://localhost", APIKey: "hlk_test", Priority: -1}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for negative priority")
	}
}

func TestValidate_PriorityTooHigh(t *testing.T) {
	cfg := PushWardConfig{URL: "http://localhost", APIKey: "hlk_test", Priority: 11}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for priority > 10")
	}
}

func TestValidate_PriorityBoundaries(t *testing.T) {
	for _, p := range []int{0, 10} {
		cfg := PushWardConfig{URL: "http://localhost", APIKey: "hlk_test", Priority: p}
		if err := cfg.Validate(); err != nil {
			t.Errorf("priority %d should be valid, got %v", p, err)
		}
	}
}
