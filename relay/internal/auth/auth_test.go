package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// basicHeader builds a "Basic <base64(user:pass)>" Authorization header value.
func basicHeader(scheme, user, pass string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	return scheme + " " + enc
}

func TestExtractKey(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		// Bearer scheme is case-insensitive (RFC 7235).
		{"bearer canonical", "Bearer hlk_x", "hlk_x"},
		{"bearer lowercase", "bearer hlk_x", "hlk_x"},
		{"bearer uppercase", "BEARER hlk_x", "hlk_x"},
		{"bearer mixed case", "BeArEr hlk_x", "hlk_x"},
		{"bearer non-hlk token", "Bearer abc123", ""},
		{"bearer empty token", "Bearer ", ""},

		// Basic scheme: hlk_ taken from the password field, scheme case-insensitive.
		{"basic canonical password", basicHeader("Basic", "user", "hlk_x"), "hlk_x"},
		{"basic lowercase scheme", basicHeader("basic", "user", "hlk_x"), "hlk_x"},
		{"basic empty username", basicHeader("Basic", "", "hlk_x"), "hlk_x"},
		{"basic non-hlk password", basicHeader("Basic", "user", "secret"), ""},
		{"basic no colon", "Basic " + base64.StdEncoding.EncodeToString([]byte("nopassword")), ""},
		{"basic invalid base64", "Basic not_base64!!!", ""},

		// GenieKey scheme (OpsGenie, used by TrueNAS), scheme case-insensitive.
		{"geniekey canonical", "GenieKey hlk_x", "hlk_x"},
		{"geniekey lowercase", "geniekey hlk_x", "hlk_x"},
		{"geniekey mixed case", "GenIeKeY hlk_x", "hlk_x"},
		{"geniekey non-hlk token", "GenieKey abc123", ""},
		{"geniekey empty token", "GenieKey ", ""},

		// Missing / malformed headers.
		{"empty header", "", ""},
		{"no space", "Bearer", ""},
		{"unknown scheme", "Token hlk_x", ""},
		{"garbage", "garbage", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractKey(tt.header); got != tt.want {
				t.Errorf("ExtractKey(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

// The short hashes are prefixes of the digest, so log and map correlation
// still lines up with what the state store persists.
func TestKeyDigest(t *testing.T) {
	d := KeyDigest("hlk_x")
	got := hex.EncodeToString(d[:])
	if want := "49eb6901026039ed2364e7596baf384161b9182b21795240fc014dc1b5665737"; got != want {
		t.Fatalf("KeyDigest(hlk_x) = %s, want %s", got, want)
	}
	if !strings.HasPrefix(got, KeyHash("hlk_x")) || !strings.HasPrefix(got, MapKeyPrefix("hlk_x")) {
		t.Errorf("KeyHash/MapKeyPrefix are not prefixes of KeyDigest %s", got)
	}
}

func TestUniversalDigest(t *testing.T) {
	d := UniversalDigest("hlk_x")
	if d == KeyDigest("hlk_x") {
		t.Fatal("UniversalDigest must differ from KeyDigest")
	}
	want := sha256.Sum256([]byte("pushward-relay/universal/v1\x00hlk_x"))
	if d != want {
		t.Errorf("UniversalDigest(hlk_x) = %x, want %x", d, want)
	}
	if UniversalDigest("hlk_y") == d {
		t.Error("two keys share a digest")
	}
}
