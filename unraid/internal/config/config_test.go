package config

import (
	"os"
	"path/filepath"
	"testing"
)

// clearUnraidEnv unsets every env var Load reads from, including bare-name fallbacks,
// so tests in this package don't leak env state into one another.
func clearUnraidEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PUSHWARD_UNRAID_HOST", "UNRAID_HOST",
		"PUSHWARD_UNRAID_PORT", "UNRAID_PORT",
		"PUSHWARD_UNRAID_API_KEY", "UNRAID_API_KEY",
		"PUSHWARD_UNRAID_USE_TLS", "UNRAID_USE_TLS",
		"PUSHWARD_UNRAID_SERVER_NAME", "UNRAID_SERVER_NAME",
		"PUSHWARD_URL", "PUSHWARD_API_KEY",
	} {
		_ = os.Unsetenv(k)
	}
}

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestLoad_DefaultsHostToLocalhost(t *testing.T) {
	clearUnraidEnv(t)
	t.Setenv("PUSHWARD_UNRAID_API_KEY", "unraid-key")
	t.Setenv("PUSHWARD_URL", "http://pw")
	t.Setenv("PUSHWARD_API_KEY", "hlk_test")

	cfg, err := Load("/nonexistent.yml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Unraid.Host != "localhost" {
		t.Errorf("expected default host 'localhost', got %q", cfg.Unraid.Host)
	}
	if cfg.Unraid.Port != 80 {
		t.Errorf("expected default port 80, got %d", cfg.Unraid.Port)
	}
	if cfg.Unraid.ServerName != "Unraid" {
		t.Errorf("expected default server_name 'Unraid', got %q", cfg.Unraid.ServerName)
	}
	if cfg.Unraid.UseTLS {
		t.Error("expected use_tls=false by default")
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	clearUnraidEnv(t)
	t.Setenv("PUSHWARD_UNRAID_HOST", "tower.local")
	t.Setenv("PUSHWARD_UNRAID_PORT", "443")
	t.Setenv("PUSHWARD_UNRAID_API_KEY", "unraid-key")
	t.Setenv("PUSHWARD_UNRAID_USE_TLS", "true")
	t.Setenv("PUSHWARD_UNRAID_SERVER_NAME", "Tower")
	t.Setenv("PUSHWARD_URL", "https://api.pushward.app")
	t.Setenv("PUSHWARD_API_KEY", "hlk_env")

	cfg, err := Load("/nonexistent.yml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Unraid.Host != "tower.local" {
		t.Errorf("host = %q", cfg.Unraid.Host)
	}
	if cfg.Unraid.Port != 443 {
		t.Errorf("port = %d", cfg.Unraid.Port)
	}
	if !cfg.Unraid.UseTLS {
		t.Error("use_tls should be true")
	}
	if cfg.Unraid.ServerName != "Tower" {
		t.Errorf("server_name = %q", cfg.Unraid.ServerName)
	}
}

func TestLoad_UseTLS_Falsey(t *testing.T) {
	for _, v := range []string{"false", "False", "FALSE", "0", "f", "F"} {
		t.Run(v, func(t *testing.T) {
			clearUnraidEnv(t)
			t.Setenv("PUSHWARD_UNRAID_API_KEY", "unraid-key")
			t.Setenv("PUSHWARD_UNRAID_USE_TLS", v)
			t.Setenv("PUSHWARD_URL", "http://pw")
			t.Setenv("PUSHWARD_API_KEY", "hlk_test")
			cfg, err := Load("/nonexistent.yml")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Unraid.UseTLS {
				t.Errorf("use_tls=%q should be false", v)
			}
		})
	}
}

func TestLoad_UseTLS_Invalid(t *testing.T) {
	clearUnraidEnv(t)
	t.Setenv("PUSHWARD_UNRAID_API_KEY", "unraid-key")
	t.Setenv("PUSHWARD_UNRAID_USE_TLS", "maybe")
	t.Setenv("PUSHWARD_URL", "http://pw")
	t.Setenv("PUSHWARD_API_KEY", "hlk_test")
	if _, err := Load("/nonexistent.yml"); err == nil {
		t.Fatal("expected error for invalid PUSHWARD_UNRAID_USE_TLS")
	}
}

func TestLoad_BareEnvFallback(t *testing.T) {
	clearUnraidEnv(t)
	t.Setenv("UNRAID_USE_TLS", "true")
	t.Setenv("UNRAID_SERVER_NAME", "BareName")
	t.Setenv("PUSHWARD_UNRAID_API_KEY", "unraid-key")
	t.Setenv("PUSHWARD_URL", "http://pw")
	t.Setenv("PUSHWARD_API_KEY", "hlk_test")

	cfg, err := Load("/nonexistent.yml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Unraid.UseTLS {
		t.Error("UNRAID_USE_TLS bare fallback should set use_tls")
	}
	if cfg.Unraid.ServerName != "BareName" {
		t.Errorf("UNRAID_SERVER_NAME bare fallback failed, got %q", cfg.Unraid.ServerName)
	}
}

func TestLoad_YAMLOverridesDefaults(t *testing.T) {
	clearUnraidEnv(t)
	path := writeYAML(t, `
unraid:
  host: yaml-host
  port: 8443
  api_key: yaml-key
  server_name: YamlName
  use_tls: true
pushward:
  url: http://pw
  api_key: hlk_yaml
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Unraid.Host != "yaml-host" || cfg.Unraid.Port != 8443 ||
		cfg.Unraid.ServerName != "YamlName" || !cfg.Unraid.UseTLS {
		t.Errorf("YAML did not override defaults: %+v", cfg.Unraid)
	}
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	clearUnraidEnv(t)
	t.Setenv("PUSHWARD_UNRAID_HOST", "env-host")
	t.Setenv("PUSHWARD_UNRAID_API_KEY", "env-key")
	t.Setenv("PUSHWARD_URL", "http://pw")
	t.Setenv("PUSHWARD_API_KEY", "hlk_env")
	path := writeYAML(t, `
unraid:
  host: yaml-host
  api_key: yaml-key
pushward:
  url: http://yaml
  api_key: hlk_yaml
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Unraid.Host != "env-host" {
		t.Errorf("env should win, got %q", cfg.Unraid.Host)
	}
}

func TestLoad_MissingAPIKey(t *testing.T) {
	clearUnraidEnv(t)
	t.Setenv("PUSHWARD_URL", "http://pw")
	t.Setenv("PUSHWARD_API_KEY", "hlk_test")
	if _, err := Load("/nonexistent.yml"); err == nil {
		t.Fatal("expected error for missing Unraid API key")
	}
}

func TestLoad_InvalidPort(t *testing.T) {
	clearUnraidEnv(t)
	t.Setenv("PUSHWARD_UNRAID_PORT", "not-a-number")
	t.Setenv("PUSHWARD_UNRAID_API_KEY", "unraid-key")
	t.Setenv("PUSHWARD_URL", "http://pw")
	t.Setenv("PUSHWARD_API_KEY", "hlk_test")
	if _, err := Load("/nonexistent.yml"); err == nil {
		t.Fatal("expected error for invalid PUSHWARD_UNRAID_PORT")
	}
}
