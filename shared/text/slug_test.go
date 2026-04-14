package text

import (
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		input  string
		want   string
	}{
		{"empty input", "app-", "", "app-"},
		{"simple lowercase", "app-", "hello", "app-hello"},
		{"mixed case", "app-", "HelloWorld", "app-helloworld"},
		{"spaces", "app-", "hello world", "app-hello-world"},
		{"special chars", "app-", "hello!@#world", "app-hello-world"},
		{"leading/trailing non-alnum", "app-", "!!hello!!", "app-hello"},
		{"runs of separators", "app-", "a___b---c", "app-a-b-c"},
		{"all non-alnum", "app-", "!@#$%", "app-"},
		{"unicode letters", "app-", "café", "app-caf"},
		{"digits preserved", "app-", "item 42", "app-item-42"},
		{"no prefix", "", "hello world", "hello-world"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Slug(tt.prefix, tt.input)
			if got != tt.want {
				t.Errorf("Slug(%q, %q) = %q, want %q", tt.prefix, tt.input, got, tt.want)
			}
		})
	}
}

func TestSlugHash(t *testing.T) {
	tests := []struct {
		name      string
		prefix    string
		input     string
		hashBytes int
		wantLen   int
	}{
		// format is "<prefix>-<hex>" so length = len(prefix) + 1 + hashBytes*2
		{"empty input 4 bytes", "app", "", 4, len("app") + 1 + 8},
		{"simple 4 bytes", "app", "hello", 4, len("app") + 1 + 8},
		{"8 bytes", "app", "hello", 8, len("app") + 1 + 16},
		{"0 bytes", "app", "hello", 0, len("app") + 1},
		{"no prefix", "", "hello", 4, 1 + 8},
		{"full 32 bytes", "app", "hello", 32, len("app") + 1 + 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SlugHash(tt.prefix, tt.input, tt.hashBytes)
			if len(got) != tt.wantLen {
				t.Errorf("SlugHash(%q, %q, %d) length = %d, want %d (got %q)",
					tt.prefix, tt.input, tt.hashBytes, len(got), tt.wantLen, got)
			}
			wantPrefix := tt.prefix + "-"
			if !strings.HasPrefix(got, wantPrefix) {
				t.Errorf("SlugHash(%q, %q, %d) = %q, missing prefix %q",
					tt.prefix, tt.input, tt.hashBytes, got, wantPrefix)
			}
		})
	}
}

func TestSlugHash_Deterministic(t *testing.T) {
	a := SlugHash("app-", "same-input", 8)
	b := SlugHash("app-", "same-input", 8)
	if a != b {
		t.Errorf("SlugHash is not deterministic: %q != %q", a, b)
	}
}

func TestSlugHash_DifferentInputs(t *testing.T) {
	a := SlugHash("app-", "input-a", 8)
	b := SlugHash("app-", "input-b", 8)
	if a == b {
		t.Errorf("different inputs should produce different hashes, got %q for both", a)
	}
}
