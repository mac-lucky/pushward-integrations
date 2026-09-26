package universal

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func flatten(t *testing.T, body string) ([]Field, bool) {
	t.Helper()
	fields, truncated, err := Flatten(strings.NewReader(body))
	if err != nil {
		t.Fatalf("Flatten(%s): %v", body, err)
	}
	return fields, truncated
}

func paths(fields []Field) []string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = f.Path
	}
	return out
}

func TestFlattenLeaves(t *testing.T) {
	fields, truncated := flatten(t, `{
		"status": "firing",
		"count": 3,
		"ok": false,
		"note": null,
		"labels": {"severity": "critical"},
		"alerts": [{"name": "a", "extra": 1}, {"name": "b", "other": 2}],
		"tags": ["x", "y"],
		"empty": [],
		"nothing": {}
	}`)
	if truncated {
		t.Error("small payload reported as truncated")
	}
	want := []Field{
		{"status", "firing", TypeString},
		{"count", "3", TypeNumber},
		{"ok", "false", TypeBool},
		{"note", "", TypeNull},
		{"labels.severity", "critical", TypeString},
		{"alerts[].name", "a", TypeString},
		{"alerts[].extra", "1", TypeNumber},
		{"tags[]", "x", TypeString},
		{"empty[]", "", TypeEmpty},
	}
	if len(fields) != len(want) {
		t.Fatalf("got %v, want %v", fields, want)
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Errorf("field %d = %+v, want %+v", i, fields[i], want[i])
		}
	}
}

func TestFlattenTopLevel(t *testing.T) {
	fields, _ := flatten(t, `[{"id": 1}, {"id": 2, "late": true}]`)
	if got := strings.Join(paths(fields), ","); got != "[].id" {
		t.Errorf("paths = %s, want [].id", got)
	}
	for _, body := range []string{`"text"`, `42`, `null`} {
		if _, _, err := Flatten(strings.NewReader(body)); err == nil {
			t.Errorf("Flatten(%s): want an error for a bare scalar", body)
		}
	}
	if _, _, err := Flatten(strings.NewReader(`{"a": `)); err == nil {
		t.Error("want an error for a cut-off body")
	}
}

func TestFlattenDepthCap(t *testing.T) {
	// Ten nested objects: keys k1..k8 are walked, anything deeper is skipped
	// without failing the decode.
	body := `{"top": 1, ` + strings.Repeat(`"k": {`, 10) + `"leaf": 1` + strings.Repeat(`}`, 10) + `, "after": 2}`
	fields, _ := flatten(t, body)
	for _, f := range fields {
		if n := strings.Count(f.Path, ".") + 1; n > MaxDepth {
			t.Errorf("path %s has %d segments, cap is %d", f.Path, n, MaxDepth)
		}
	}
	got := strings.Join(paths(fields), ",")
	if got != "top,after" {
		t.Errorf("paths = %s, want the deep chain dropped and its siblings kept", got)
	}

	fields, _ = flatten(t, `{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":1}}}}}}}}`)
	if got := strings.Join(paths(fields), ","); got != "a.b.c.d.e.f.g.h" {
		t.Errorf("a leaf at exactly MaxDepth was dropped: %q", got)
	}
}

func TestFlattenPathCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("{")
	for i := range MaxPaths + 50 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"key_%c%c": %d`, 'a'+i/26%26, 'a'+i%26, i)
	}
	b.WriteString("}")
	fields, truncated := flatten(t, b.String())
	if !truncated {
		t.Error("truncated = false past the path cap")
	}
	if len(fields) != MaxPaths {
		t.Errorf("got %d fields, want %d", len(fields), MaxPaths)
	}
}

func TestFlattenValueCap(t *testing.T) {
	long := strings.Repeat(string(rune(0xe9)), MaxValueRunes+40) // two bytes per rune
	fields, _ := flatten(t, `{"text": "`+long+`"}`)
	v := fields[0].Value
	if n := utf8.RuneCountInString(v); n != MaxValueRunes {
		t.Errorf("value has %d runes, want %d", n, MaxValueRunes)
	}
	if !utf8.ValidString(v) {
		t.Error("capped value cut a rune in half")
	}
}

func TestNormalizeKey(t *testing.T) {
	cases := map[string]string{
		"3f2b8c1e-9d4a-4b7e-8c21-5a6f0e9d1b2c": "*",
		"12345":                                "*",
		"-7":                                   "*",
		"2026-09-26":                           "*",
		"2026-09-26T10:15:00Z":                 "*",
		"6650f1c2a1b2c3d4e5f60789":             "*",
		"a1b2c3d4":                             "*",
		"deadbeef":                             "deadbeef",
		"status":                               "status",
		"v2":                                   "v2",
		"abc123":                               "abc123",
		"html_url":                             "html_url",
	}
	for in, want := range cases {
		if got := NormalizeKey(in); got != want {
			t.Errorf("NormalizeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFlattenIDKeyedMap(t *testing.T) {
	fields, _ := flatten(t, `{"checks": {
		"3f2b8c1e-9d4a-4b7e-8c21-5a6f0e9d1b2c": {"status": "up"},
		"8a1c2d3e-0000-4b7e-8c21-5a6f0e9d1b2c": {"status": "down", "extra": 1}
	}}`)
	if got := strings.Join(paths(fields), ","); got != "checks.*.status" {
		t.Errorf("paths = %s, want only the first id-keyed member walked", got)
	}
}
