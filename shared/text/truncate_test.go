package text_test

import (
	"testing"

	"github.com/mac-lucky/pushward-integrations/shared/text"
)

func TestTruncate_Short(t *testing.T) {
	if got := text.Truncate("hello", 10); got != "hello" {
		t.Errorf("text.Truncate(hello, 10) = %q, want hello", got)
	}
}

func TestTruncate_ExactLength(t *testing.T) {
	if got := text.Truncate("hello", 5); got != "hello" {
		t.Errorf("text.Truncate(hello, 5) = %q, want hello", got)
	}
}

func TestTruncate_LongASCII(t *testing.T) {
	got := text.Truncate("Hello World!", 8)
	if got != "Hello..." {
		t.Errorf("text.Truncate(Hello World!, 8) = %q, want Hello...", got)
	}
}

func TestTruncate_UTF8(t *testing.T) {
	// 5 rune string: "héllo"
	got := text.Truncate("héllo world", 8)
	if got != "héllo..." {
		t.Errorf("text.Truncate(héllo world, 8) = %q, want héllo...", got)
	}
}

func TestTruncate_CJK(t *testing.T) {
	// CJK characters: each is one rune
	input := "你好世界测试字符串"
	got := text.Truncate(input, 6)
	// 6 runes: first 3 + "..."
	if got != "你好世..." {
		t.Errorf("truncate CJK = %q, want 你好世...", got)
	}
}

func TestTruncate_MaxLen3(t *testing.T) {
	got := text.Truncate("Hello World", 3)
	// maxLen <= 3: no ellipsis, just first 3 runes
	if got != "Hel" {
		t.Errorf("text.Truncate(Hello World, 3) = %q, want Hel", got)
	}
}

func TestTruncate_MaxLen1(t *testing.T) {
	got := text.Truncate("Hello", 1)
	if got != "H" {
		t.Errorf("text.Truncate(Hello, 1) = %q, want H", got)
	}
}

func TestTruncate_Emoji(t *testing.T) {
	input := "🎉🎊🎈🎁🎀🎗"
	got := text.Truncate(input, 5)
	if got != "🎉🎊..." {
		t.Errorf("truncate emoji = %q, want 🎉🎊...", got)
	}
}

func TestTruncate_MaxLenZero(t *testing.T) {
	if got := text.Truncate("hello", 0); got != "" {
		t.Errorf("text.Truncate(hello, 0) = %q, want empty", got)
	}
}

func TestTruncate_MaxLenNegative(t *testing.T) {
	if got := text.Truncate("hello", -1); got != "" {
		t.Errorf("text.Truncate(hello, -1) = %q, want empty", got)
	}
}

func TestTruncateHard_MaxLenZero(t *testing.T) {
	if got := text.TruncateHard("hello", 0); got != "" {
		t.Errorf("text.TruncateHard(hello, 0) = %q, want empty", got)
	}
}

func TestTruncateHard_MaxLenNegative(t *testing.T) {
	if got := text.TruncateHard("hello", -1); got != "" {
		t.Errorf("text.TruncateHard(hello, -1) = %q, want empty", got)
	}
}
