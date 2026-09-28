package universal

import (
	"fmt"
	"math/rand/v2"
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
		"anna@example.com":                     "*",
		"+48 123 456 789":                      "*",
		"eyJhbGciOiJIUzI1NiJ9":                 "*",
		"hlk_3f2b8c1e9d4a":                     "*",
		testRandom:                             "*",
		"custom.cf_" + testRandom:              "*",
		// Long, but words: entropy alone would have collapsed these.
		"is_auto_renew_enabled_on_trial_end": "is_auto_renew_enabled_on_trial_end",
		"long_description_link_url_3":        "long_description_link_url_3",
		"shippingAddressStreet2":             "shippingAddressStreet2",
		"payload[order][customer][id]":       "payload[order][customer][id]",
		"X-Amz-Content-SHA256":               "X-Amz-Content-SHA256",
		"+48":                                "+48",
		"tblQx7Rk2Vb9Nm4Zc":                  "*",
		"user:anna@example.com":              "*",
		"(415) 555-1234":                     "*",
		"Anna <anna@example.com>":            "*",
		"415-555-1234":                       "*",
		"123-45-6789":                        "*",
		"4111 1111 1111 1111":                "*",
		"q7Xk-2Vb9_Rt4N-m8Zc3W":              "*",
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

func TestFlattenPathBytesCap(t *testing.T) {
	fit := strings.Repeat("k", MaxPathBytes)
	fields, truncated := flatten(t, `{"`+fit+`": 1}`)
	if truncated || len(fields) != 1 {
		t.Errorf("a %d-byte path was dropped: %v truncated=%v", MaxPathBytes, paths(fields), truncated)
	}

	over := strings.Repeat("k", MaxPathBytes-1)
	fields, truncated = flatten(t, `{
		"a": {"`+over+`": {"deep": 1}, "b": [1]},
		"c": {"`+over[:MaxPathBytes-3]+`": [1]},
		"ok": 2
	}`)
	if !truncated {
		t.Error("truncated = false after dropping a long path")
	}
	if got := strings.Join(paths(fields), ","); got != "a.b[],ok" {
		t.Errorf("paths = %s, want the long subtrees gone and their siblings kept", got)
	}
	for _, f := range fields {
		if len(f.Path) > MaxPathBytes {
			t.Errorf("path of %d bytes", len(f.Path))
		}
	}
}

// A key too long for any path is skipped before it is looked at.
func TestFlattenHugeKey(t *testing.T) {
	huge := strings.Repeat("k", 1<<20)
	fields, truncated := flatten(t, `{"`+huge+`": {"a": 1}, "ok": 2}`)
	if !truncated || strings.Join(paths(fields), ",") != "ok" {
		t.Errorf("paths = %v truncated = %v, want the huge key skipped", paths(fields), truncated)
	}
}

// Generated keys collapse to "*" often enough to keep fingerprints stable;
// the stricter key rules let some through rather than collapse a real name.
func TestNormalizeKeyGenerated(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4)) // #nosec G404 -- a fixed seed, so the rates are stable
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	for _, c := range []struct {
		alphabet string
		n, most  int // most keys of 1000 kept as they are
	}{{alnum, 32, 120}, {alnum + "-_", 43, 120}, {alnum, 20, 220}} {
		kept := 0
		for range 1000 {
			b := make([]byte, c.n)
			for i := range b {
				b[i] = c.alphabet[rng.IntN(len(c.alphabet))]
			}
			if NormalizeKey(string(b)) != "*" {
				kept++
			}
		}
		if kept > c.most {
			t.Errorf("%d random keys of %d characters over %d symbols kept, want at most %d", kept, c.n, len(c.alphabet), c.most)
		}
	}
}
