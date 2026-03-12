package extract

import "testing"

func TestGet_Simple(t *testing.T) {
	data := map[string]any{"status": "running"}
	val, ok := Get(data, "status")
	if !ok {
		t.Fatal("expected ok")
	}
	if val != "running" {
		t.Fatalf("expected running, got %v", val)
	}
}

func TestGet_Nested(t *testing.T) {
	data := map[string]any{
		"print": map[string]any{
			"status": map[string]any{
				"percent": 42.5,
			},
		},
	}
	val, ok := Get(data, "print.status.percent")
	if !ok {
		t.Fatal("expected ok")
	}
	if val != 42.5 {
		t.Fatalf("expected 42.5, got %v", val)
	}
}

func TestGet_Missing(t *testing.T) {
	data := map[string]any{"status": "ok"}
	_, ok := Get(data, "missing")
	if ok {
		t.Fatal("expected not ok")
	}
	_, ok = Get(data, "status.nested")
	if ok {
		t.Fatal("expected not ok for non-map access")
	}
}

func TestGetString(t *testing.T) {
	data := map[string]any{
		"name":    "washer",
		"count":   float64(42),
		"ratio":   3.14,
		"enabled": true,
	}

	tests := []struct {
		path string
		want string
		ok   bool
	}{
		{"name", "washer", true},
		{"count", "42", true},
		{"ratio", "3.14", true},
		{"enabled", "true", true},
		{"missing", "", false},
	}

	for _, tt := range tests {
		got, ok := GetString(data, tt.path)
		if ok != tt.ok {
			t.Errorf("GetString(%q): ok = %v, want %v", tt.path, ok, tt.ok)
		}
		if got != tt.want {
			t.Errorf("GetString(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestGetFloat64(t *testing.T) {
	data := map[string]any{
		"percent": float64(85),
		"text":    "3.14",
		"name":    "washer",
	}

	f, ok := GetFloat64(data, "percent")
	if !ok || f != 85 {
		t.Errorf("GetFloat64(percent) = %v, %v", f, ok)
	}

	f, ok = GetFloat64(data, "text")
	if !ok || f != 3.14 {
		t.Errorf("GetFloat64(text) = %v, %v", f, ok)
	}

	_, ok = GetFloat64(data, "name")
	if ok {
		t.Error("expected GetFloat64(name) to fail")
	}
}

func TestGetInt(t *testing.T) {
	data := map[string]any{
		"count": float64(42),
		"text":  "7",
		"ratio": float64(3.9),
	}

	i, ok := GetInt(data, "count")
	if !ok || i != 42 {
		t.Errorf("GetInt(count) = %v, %v", i, ok)
	}

	i, ok = GetInt(data, "text")
	if !ok || i != 7 {
		t.Errorf("GetInt(text) = %v, %v", i, ok)
	}

	i, ok = GetInt(data, "ratio")
	if !ok || i != 3 {
		t.Errorf("GetInt(ratio) = %v, %v", i, ok)
	}
}
