package universalhook

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	tokenKey = bytes.Repeat([]byte{0x5a}, 32)
	tokenNow = time.Unix(1_790_000_000, 0)
)

func testClaims(s Scope) Claims {
	c := Claims{Scope: s, Expires: tokenNow.Add(time.Hour)}
	for i := range c.KeyHash {
		c.KeyHash[i] = byte(i)
	}
	if s != ScopeList {
		for i := range c.Fingerprint {
			c.Fingerprint[i] = byte(255 - i)
		}
		c.Source = "alertmanager-prod"
	}
	return c
}

func mustMint(t *testing.T, c Claims) string {
	t.Helper()
	tok, err := Mint(tokenKey, c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestTokenRoundTrip(t *testing.T) {
	for _, s := range []Scope{ScopeAccept, ScopeReject, ScopeEdit, ScopeList} {
		t.Run(string(s), func(t *testing.T) {
			want := testClaims(s)
			tok := mustMint(t, want)
			if len(tok) > maxTokenLen {
				t.Errorf("token is %d characters, over maxTokenLen %d", len(tok), maxTokenLen)
			}
			got, err := Parse(tokenKey, tok, tokenNow, s)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("Parse = %+v, want %+v", got, want)
			}
		})
	}
}

// A source of the full 32 characters is the longest token there is.
func TestTokenLongestSource(t *testing.T) {
	c := testClaims(ScopeEdit)
	c.Source = strings.Repeat("a", maxSourceLen)
	tok := mustMint(t, c)
	if len(tok) > maxTokenLen {
		t.Fatalf("token is %d characters, over maxTokenLen %d", len(tok), maxTokenLen)
	}
	if _, err := Parse(tokenKey, tok, tokenNow, ScopeEdit); err != nil {
		t.Fatal(err)
	}
}

// Every changed byte breaks the mac, and a broken mac is not found, never
// expired: an attacker must not learn which tokens once existed.
func TestTokenTamper(t *testing.T) {
	for _, s := range []Scope{ScopeAccept, ScopeEdit, ScopeList} {
		raw, err := b64.DecodeString(mustMint(t, testClaims(s)))
		if err != nil {
			t.Fatal(err)
		}
		for i := range raw {
			for _, flip := range []byte{0x01, 0x80} {
				b := bytes.Clone(raw)
				b[i] ^= flip
				if _, err := Parse(tokenKey, b64.EncodeToString(b), tokenNow, s, ScopeAccept, ScopeReject, ScopeEdit, ScopeList); !errors.Is(err, ErrNotFound) {
					t.Errorf("scope %c: byte %d ^ %#x: err = %v, want ErrNotFound", s, i, flip, err)
				}
			}
		}
		// Cut or extended tokens.
		for _, b := range [][]byte{raw[:len(raw)-1], raw[1:], append(bytes.Clone(raw), 0)} {
			if _, err := Parse(tokenKey, b64.EncodeToString(b), tokenNow, s); !errors.Is(err, ErrNotFound) {
				t.Errorf("scope %c: resized token: err = %v, want ErrNotFound", s, err)
			}
		}
	}
}

// Changing any character of the text must not produce another valid token;
// the strict decoding keeps the last character from having spare bits.
func TestTokenTamperText(t *testing.T) {
	tok := mustMint(t, testClaims(ScopeReject))
	for i := range tok {
		for _, c := range []byte{'A', 'z', '-', '_', '0'} {
			if tok[i] == c {
				continue
			}
			bad := tok[:i] + string(c) + tok[i+1:]
			if _, err := Parse(tokenKey, bad, tokenNow, ScopeReject); !errors.Is(err, ErrNotFound) {
				t.Errorf("char %d -> %c: err = %v, want ErrNotFound", i, c, err)
			}
		}
	}
	for _, bad := range []string{"", "!", tok + "=", tok + "A", strings.Repeat("A", maxTokenLen+1), tok[:10]} {
		if _, err := Parse(tokenKey, bad, tokenNow, ScopeReject); !errors.Is(err, ErrNotFound) {
			t.Errorf("Parse(%.20q...) err = %v, want ErrNotFound", bad, err)
		}
	}
}

func TestTokenWrongKey(t *testing.T) {
	tok := mustMint(t, testClaims(ScopeAccept))
	other := bytes.Repeat([]byte{0x5b}, 32)
	if _, err := Parse(other, tok, tokenNow, ScopeAccept); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestTokenExpired(t *testing.T) {
	want := testClaims(ScopeAccept)
	tok := mustMint(t, want)
	for _, now := range []time.Time{want.Expires, want.Expires.Add(time.Second), want.Expires.Add(365 * 24 * time.Hour)} {
		got, err := Parse(tokenKey, tok, now, ScopeAccept)
		if !errors.Is(err, ErrExpired) {
			t.Fatalf("at %s: err = %v, want ErrExpired", now, err)
		}
		if got != want {
			t.Errorf("an expired token still returns its claims: got %+v", got)
		}
	}
	if _, err := Parse(tokenKey, tok, want.Expires.Add(-time.Second), ScopeAccept); err != nil {
		t.Errorf("a second before expiry: %v", err)
	}
	// An expired token with a bad mac is not found: the mac is checked first.
	raw, _ := b64.DecodeString(tok)
	raw[len(raw)-1] ^= 1
	if _, err := Parse(tokenKey, b64.EncodeToString(raw), want.Expires.Add(time.Hour), ScopeAccept); !errors.Is(err, ErrNotFound) {
		t.Errorf("forged expired token: err = %v, want ErrNotFound", err)
	}
}

func TestTokenCrossScope(t *testing.T) {
	tests := []struct {
		minted  Scope
		allowed []Scope
	}{
		{ScopeAccept, []Scope{ScopeEdit}},
		{ScopeReject, []Scope{ScopeEdit, ScopeList}},
		{ScopeEdit, []Scope{ScopeAccept, ScopeReject}},
		{ScopeList, []Scope{ScopeAccept, ScopeReject, ScopeEdit}},
		{ScopeEdit, []Scope{ScopeList}},
		{ScopeAccept, []Scope{ScopeReject}},
		{ScopeReject, []Scope{ScopeAccept}},
		{ScopeAccept, nil},
	}
	for _, tt := range tests {
		tok := mustMint(t, testClaims(tt.minted))
		if _, err := Parse(tokenKey, tok, tokenNow, tt.allowed...); !errors.Is(err, ErrNotFound) {
			t.Errorf("%c token where %q is allowed: err = %v, want ErrNotFound", tt.minted, tt.allowed, err)
		}
	}
}

// A review token with its scope byte rewritten to edit, and the mac
// recomputed under the review domain, still fails: the edit domain differs.
func TestTokenDomainSeparation(t *testing.T) {
	c := testClaims(ScopeAccept)
	raw, _ := b64.DecodeString(mustMint(t, c))
	body := bytes.Clone(raw[:len(raw)-macLen])
	body[1] = byte(ScopeEdit)
	forged := append(body, mac(tokenKey, domain(ScopeAccept), body)...)
	if _, err := Parse(tokenKey, b64.EncodeToString(forged), tokenNow, ScopeEdit); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestMintRejects(t *testing.T) {
	bad := []Claims{
		{Scope: 'x', Expires: tokenNow},
		{Scope: ScopeAccept, Expires: tokenNow, Source: "Upper"},
		{Scope: ScopeAccept, Expires: tokenNow, Source: strings.Repeat("a", maxSourceLen+1)},
		{Scope: ScopeAccept, Expires: time.Unix(0, 0)},
		{Scope: ScopeAccept, Expires: time.Unix(1<<33, 0)},
	}
	for _, c := range bad {
		if _, err := Mint(tokenKey, c); err == nil {
			t.Errorf("Mint(%+v) succeeded", c)
		}
	}
}
