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
		// Non-empty input that collapses to empty falls back to a hash so
		// distinct such inputs don't all share one slug (collision hazard). The
		// prefix supplies the only separator, so there is no double hyphen.
		{"all non-alnum", "app-", "!@#$%", "app-" + HashHex("!@#$%", 8)},
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

// Distinct inputs that both normalize to an empty body (CJK-only, symbol-only)
// must produce distinct slugs rather than colliding on the prefix.
func TestSlug_NonEmptyCollapsingIsDistinct(t *testing.T) {
	a := Slug("bazarr-", "日本語")
	b := Slug("bazarr-", "!!!")
	if a == b {
		t.Fatalf("distinct collapsing inputs collided: both -> %q", a)
	}
	if !strings.HasPrefix(a, "bazarr-") || !strings.HasPrefix(b, "bazarr-") {
		t.Errorf("slugs lost their prefix: %q, %q", a, b)
	}
	// The prefix carries the only separator: no double hyphen at the join, and
	// (empty prefix) no leading hyphen that would violate the slug pattern.
	if strings.Contains(a, "--") || strings.Contains(b, "--") {
		t.Errorf("fallback emitted a double hyphen: %q, %q", a, b)
	}
	if got := Slug("", "日本語"); strings.HasPrefix(got, "-") {
		t.Errorf("empty-prefix fallback emitted a leading hyphen: %q", got)
	}
	// Genuinely-empty input still returns just the prefix (nothing to hash).
	if got := Slug("bazarr-", ""); got != "bazarr-" {
		t.Errorf("empty input = %q, want %q", got, "bazarr-")
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

func TestHashHex(t *testing.T) {
	// Distinguishing contract vs SlugHash: hex only, no separator, length 2*n.
	got := HashHex("hello", 8)
	if len(got) != 16 {
		t.Errorf("HashHex(_, 8) length = %d, want 16 (got %q)", len(got), got)
	}
	if strings.ContainsAny(got, "-g") || strings.Trim(got, "0123456789abcdef") != "" {
		t.Errorf("HashHex emitted non-hex/separator chars: %q", got)
	}
	if HashHex("hello", 8) != got {
		t.Error("HashHex is not deterministic")
	}
	if HashHex("world", 8) == got {
		t.Error("distinct inputs should hash differently")
	}
	// SlugHash is HashHex plus the "<prefix>-" join.
	if want := "app-" + HashHex("hello", 8); SlugHash("app", "hello", 8) != want {
		t.Errorf("SlugHash should equal prefix + \"-\" + HashHex: got %q want %q", SlugHash("app", "hello", 8), want)
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
