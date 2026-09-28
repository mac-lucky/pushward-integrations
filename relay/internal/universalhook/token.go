package universalhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
)

// Scope is what a token lets its holder do. It is the token's second byte.
type Scope byte

const (
	ScopeAccept Scope = 'a'
	ScopeReject Scope = 'r'
	ScopeEdit   Scope = 'e'
	ScopeList   Scope = 'l'
)

// Token lifetimes. A review token (accept or reject) expires with the pending
// row it decides, state.PendingTTL after the proposal. An edit link on a
// notification lasts EditTokenTTL; one on the list page ListEditTokenTTL from
// when the list was opened, and never past the list link's own expiry.
const (
	EditTokenTTL     = 30 * 24 * time.Hour
	ListTokenTTL     = 24 * time.Hour
	ListEditTokenTTL = 24 * time.Hour
)

// Errors Parse returns. Every token that is not one the relay minted for this
// use is ErrNotFound, so a probe cannot tell a forged link from a deleted
// mapping. ErrExpired is kept for authentic tokens: the user tapped a real,
// old link and should be told so.
var (
	ErrNotFound = errors.New("universalhook: no such link")
	ErrExpired  = errors.New("universalhook: link expired")
)

// A token is base64url, unpadded, of
//
//	ver(1)=1 | scope(1) | exp(4, unix seconds, big endian) | key_hash(32)
//	    | fingerprint(32) | src_len(1) | source(src_len)          (a, r, e)
//	    |                                                        (l)
//	| mac(16)
//
// A list token names a tenant only, so it stops after key_hash. mac is the
// first 16 bytes of HMAC-SHA256 under the review key over the scope's domain
// followed by every byte before the mac; the domain keeps a token minted for
// one page from verifying on another even if its layout ever matched.
const (
	tokenVersion = 1
	macLen       = 16
	maxSourceLen = 32

	headerLen   = 1 + 1 + 4
	listLen     = headerLen + 32
	mappingLen  = headerLen + 32 + 32 + 1
	maxTokenLen = (mappingLen + maxSourceLen + macLen + 2) / 3 * 4
)

var sourcePattern = regexp.MustCompile(`^[a-z0-9-]{0,32}$`)

// b64 is strict so that every token has exactly one spelling.
var b64 = base64.RawURLEncoding.Strict()

func domain(s Scope) string {
	switch s {
	case ScopeAccept, ScopeReject:
		return "pushward-relay/universal-review/v1"
	case ScopeEdit:
		return "pushward-relay/universal-edit/v1"
	case ScopeList:
		return "pushward-relay/universal-list/v1"
	}
	return ""
}

// Claims is what a token carries.
type Claims struct {
	Scope   Scope
	Expires time.Time
	KeyHash [32]byte
	// Fingerprint and Source name the mapping. A list token has neither.
	Fingerprint [32]byte
	Source      string
}

// MappingKey returns the mapping a review or edit token is for.
func (c Claims) MappingKey() state.MappingKey {
	return state.MappingKey{KeyHash: c.KeyHash, Source: c.Source, Fingerprint: c.Fingerprint}
}

// Mint signs claims with key. Expires is kept to the second.
func Mint(key []byte, c Claims) (string, error) {
	d := domain(c.Scope)
	if d == "" {
		return "", fmt.Errorf("universalhook: unknown token scope %q", c.Scope)
	}
	exp := c.Expires.Unix()
	if exp <= 0 || exp > math.MaxUint32 {
		return "", fmt.Errorf("universalhook: token expiry %s out of range", c.Expires)
	}
	b := make([]byte, 0, mappingLen+maxSourceLen+macLen)
	b = append(b, tokenVersion, byte(c.Scope))
	b = binary.BigEndian.AppendUint32(b, uint32(exp)) // #nosec G115 -- range checked above
	b = append(b, c.KeyHash[:]...)
	if c.Scope != ScopeList {
		if !sourcePattern.MatchString(c.Source) {
			return "", fmt.Errorf("universalhook: invalid source %q", c.Source)
		}
		b = append(b, c.Fingerprint[:]...)
		b = append(b, byte(len(c.Source))) // #nosec G115 -- the pattern caps it at 32
		b = append(b, c.Source...)
	}
	b = append(b, mac(key, d, b)...)
	return b64.EncodeToString(b), nil
}

func mac(key []byte, domain string, body []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(domain))
	h.Write(body)
	return h.Sum(nil)[:macLen]
}

// tokenChars reports whether s is all base64url. The decoder skips \r and
// \n, so without this check one token would have many spellings.
func tokenChars(s string) bool {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// Parse verifies tok and returns its claims, if its scope is one of allowed.
// The mac is checked before anything the token says is believed, the expiry
// included; an authentic token past its expiry returns its claims with
// ErrExpired.
func Parse(key []byte, tok string, now time.Time, allowed ...Scope) (Claims, error) {
	if len(tok) > maxTokenLen || !tokenChars(tok) {
		return Claims{}, ErrNotFound
	}
	b, err := b64.DecodeString(tok)
	if err != nil || len(b) < listLen+macLen {
		return Claims{}, ErrNotFound
	}
	body, sum := b[:len(b)-macLen], b[len(b)-macLen:]
	scope := Scope(body[1])
	d := domain(scope)
	if d == "" || !hmac.Equal(sum, mac(key, d, body)) {
		return Claims{}, ErrNotFound
	}
	if body[0] != tokenVersion || !slices.Contains(allowed, scope) {
		return Claims{}, ErrNotFound
	}

	c := Claims{
		Scope:   scope,
		Expires: time.Unix(int64(binary.BigEndian.Uint32(body[2:headerLen])), 0),
	}
	copy(c.KeyHash[:], body[headerLen:listLen])
	if scope == ScopeList {
		if len(body) != listLen {
			return Claims{}, ErrNotFound
		}
	} else {
		if len(body) < mappingLen {
			return Claims{}, ErrNotFound
		}
		n := int(body[mappingLen-1])
		if len(body) != mappingLen+n {
			return Claims{}, ErrNotFound
		}
		copy(c.Fingerprint[:], body[listLen:listLen+32])
		c.Source = string(body[mappingLen:])
		if !sourcePattern.MatchString(c.Source) {
			return Claims{}, ErrNotFound
		}
	}
	if !now.Before(c.Expires) {
		return c, ErrExpired
	}
	return c, nil
}
