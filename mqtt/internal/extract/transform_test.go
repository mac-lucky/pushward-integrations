package extract

import (
	"math"
	"testing"
)

func TestDiv(t *testing.T) {
	val, err := ApplyTransform(float64(50), "div:100")
	if err != nil {
		t.Fatal(err)
	}
	if val != 0.5 {
		t.Fatalf("expected 0.5, got %v", val)
	}
}

func TestMul(t *testing.T) {
	val, err := ApplyTransform(float64(5), "mul:60")
	if err != nil {
		t.Fatal(err)
	}
	if val != float64(300) {
		t.Fatalf("expected 300, got %v", val)
	}
}

func TestFormat(t *testing.T) {
	val, err := ApplyTransform(float64(23.456), "format:%.1f°C")
	if err != nil {
		t.Fatal(err)
	}
	if val != "23.5°C" {
		t.Fatalf("expected 23.5°C, got %v", val)
	}
}

func TestScale(t *testing.T) {
	tests := []struct {
		value float64
		args  string
		want  float64
	}{
		{30, "scale:15:45", 0.5},
		{15, "scale:15:45", 0.0},
		{45, "scale:15:45", 1.0},
		{10, "scale:15:45", 0.0},  // clamped
		{50, "scale:15:45", 1.0},  // clamped
	}

	for _, tt := range tests {
		val, err := ApplyTransform(tt.value, tt.args)
		if err != nil {
			t.Fatalf("scale(%v, %s): %v", tt.value, tt.args, err)
		}
		f := val.(float64)
		if math.Abs(f-tt.want) > 0.001 {
			t.Errorf("scale(%v, %s) = %v, want %v", tt.value, tt.args, f, tt.want)
		}
	}
}

func TestDefault(t *testing.T) {
	val, err := ApplyTransform(nil, "default:Unknown")
	if err != nil {
		t.Fatal(err)
	}
	if val != "Unknown" {
		t.Fatalf("expected Unknown, got %v", val)
	}

	val, err = ApplyTransform("hello", "default:Unknown")
	if err != nil {
		t.Fatal(err)
	}
	if val != "hello" {
		t.Fatalf("expected hello, got %v", val)
	}

	val, err = ApplyTransform("", "default:fallback")
	if err != nil {
		t.Fatal(err)
	}
	if val != "fallback" {
		t.Fatalf("expected fallback for empty string, got %v", val)
	}
}

func TestUpper(t *testing.T) {
	val, err := ApplyTransform("hello", "upper")
	if err != nil {
		t.Fatal(err)
	}
	if val != "HELLO" {
		t.Fatalf("expected HELLO, got %v", val)
	}
}

func TestLower(t *testing.T) {
	val, err := ApplyTransform("HELLO", "lower")
	if err != nil {
		t.Fatal(err)
	}
	if val != "hello" {
		t.Fatalf("expected hello, got %v", val)
	}
}

func TestResolveTemplate(t *testing.T) {
	data := map[string]any{
		"running_state":         "running",
		"completion_percentage": float64(75),
		"remaining_minutes":     float64(30),
	}

	tmpl := "{running_state | upper} - {completion_percentage | div:100} - {remaining_minutes | mul:60}s"
	got := ResolveTemplate(tmpl, data)
	want := "RUNNING - 0.75 - 1800s"
	if got != want {
		t.Errorf("ResolveTemplate = %q, want %q", got, want)
	}
}

func TestResolveTemplate_MissingField(t *testing.T) {
	data := map[string]any{"name": "washer"}

	tmpl := "Status: {missing_field}"
	got := ResolveTemplate(tmpl, data)
	want := "Status: "
	if got != want {
		t.Errorf("ResolveTemplate = %q, want %q", got, want)
	}
}

func TestResolveTemplate_WithDefault(t *testing.T) {
	data := map[string]any{}

	tmpl := "{name | default:Unknown}"
	got := ResolveTemplate(tmpl, data)
	// Missing field → nil → default applies
	want := "Unknown"
	if got != want {
		t.Errorf("ResolveTemplate = %q, want %q", got, want)
	}
}
