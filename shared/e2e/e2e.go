// Package e2e seals a notification's title, subtitle, body and url into the
// pw1 end-to-end envelope. pushward-server stores and pushes only the
// ciphertext and checks the envelope's shape; the user's devices holding the
// key are the only ones that open it.
//
// testdata/vectors-v1.json is the test vector file every implementation
// (server, apps, CLI, MCP, Home Assistant and this package) passes.
package e2e

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

const (
	// MaxEnvelope is the longest envelope the server accepts.
	MaxEnvelope = 3072
	// MaxPlaintext is the most JSON that still fits in MaxEnvelope once
	// sealed.
	MaxPlaintext = 2266

	// The limits the apps clamp to after opening, in code points.
	maxTitle = 256 // subtitle too
	maxBody  = 4096
	maxURL   = 2048

	keySize   = 32
	nonceSize = 12
	tagSize   = 16
	padBlock  = 64
)

var (
	// ErrIntegrationKey is ParseKey's error for an hlk_ or hla_ key given
	// where the encryption key belongs.
	ErrIntegrationKey = errors.New("that is an integration key, not an encryption key")

	// ErrTooLong is Seal's error for a message whose JSON is over
	// MaxPlaintext. SealFit returns it only when the subtitle and url alone
	// leave no room for a title and a body.
	ErrTooLong = errors.New("too long to encrypt")

	errNoKey = errors.New("no encryption key")
)

// Key is a parsed encryption key: the AES key derived from it and its Key
// ID. The key as typed is not kept. fmt and slog print a Key as
// "e2e key <kid>".
type Key struct {
	kid string
	// enc returns the AES key. A func prints as an address with every
	// verb, so even a Key printed field by field (an unexported field,
	// where fmt cannot call String) shows no key bytes.
	enc func() []byte
}

// ParseKey reads a key in its text form, 64 hex characters as the apps show
// it. Whitespace anywhere is dropped and either case is accepted, so a key
// copied with a line break still parses. The errors never repeat the input.
func ParseKey(s string) (*Key, error) {
	h := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			return -1
		}
		return r
	}, s)
	if p := strings.ToLower(h); strings.HasPrefix(p, "hlk_") || strings.HasPrefix(p, "hla_") {
		return nil, ErrIntegrationKey
	}
	if n := utf8.RuneCountInString(h); n != 2*keySize {
		return nil, fmt.Errorf("an encryption key is %d hex characters, got %d", 2*keySize, n)
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("an encryption key is %d hex characters (0-9, a-f)", 2*keySize)
	}
	enc, err := hkdf.Key(sha256.New, raw, nil, "pushward/e2e/v1/enc", keySize)
	if err != nil {
		return nil, err
	}
	kid, err := hkdf.Key(sha256.New, raw, nil, "pushward/e2e/v1/kid", 4)
	if err != nil {
		return nil, err
	}
	return &Key{kid: hex.EncodeToString(kid), enc: func() []byte { return enc }}, nil
}

// KID is the Key ID: 8 hex characters the apps show next to the key, and the
// middle part of every envelope sealed with it. It is safe to show and log.
func (k Key) KID() string { return k.kid }

// String keeps the key material out of fmt output.
func (k Key) String() string { return "e2e key " + k.kid }

// GoString is String for %#v.
func (k Key) GoString() string { return k.String() }

// LogValue is String for slog.
func (k Key) LogValue() slog.Value { return slog.StringValue(k.String()) }

func (k *Key) aead() (cipher.AEAD, error) {
	if k == nil || k.enc == nil {
		return nil, errNoKey
	}
	block, err := aes.NewCipher(k.enc())
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Message is the sealed part of a notification. Everything else in the
// request (level, thread and collapse ids, source, icon, media, metadata,
// actions, acknowledge and tags) still travels readable, because the server
// needs it to deliver the push.
type Message struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	Body     string `json:"body"`
	URL      string `json:"url,omitempty"`
}

// Seal checks m against the limits the apps apply after opening and encrypts
// it under k with a random nonce. The JSON is padded with spaces to a
// multiple of 64 bytes, so the envelope's length says little about the text,
// unless the padding alone would push it past MaxEnvelope. JSON over
// MaxPlaintext is ErrTooLong; SealFit shortens the text instead.
func Seal(k *Key, m Message) (string, error) {
	return sealMessage(k, m, rand.Reader)
}

func sealMessage(k *Key, m Message, random io.Reader) (string, error) {
	if k == nil || k.enc == nil {
		return "", errNoKey
	}
	if err := m.validate(); err != nil {
		return "", err
	}
	p := marshal(m)
	if len(p) > MaxPlaintext {
		return "", fmt.Errorf("%w: %d bytes of JSON, at most %d fit", ErrTooLong, len(p), MaxPlaintext)
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return "", fmt.Errorf("reading nonce: %w", err)
	}
	return k.seal(nonce, pad(p))
}

// seal is the bare AES-256-GCM step: no validation, no padding. The kid is
// in the additional data, so an envelope relabeled with another kid does not
// open.
func (k *Key) seal(nonce, plaintext []byte) (string, error) {
	gcm, err := k.aead()
	if err != nil {
		return "", err
	}
	head := "pw1." + k.kid
	sealed := gcm.Seal(bytes.Clone(nonce), nonce, plaintext, []byte(head))
	return head + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// marshal is the plaintext as every implementation writes it: compact JSON
// with HTML left unescaped. Fit measures with it too, so what Go escapes
// (quotes, backslashes, control characters, U+2028 and U+2029) counts at
// its encoded size.
func marshal(m Message) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(m) // a struct of strings always encodes
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// pad appends spaces up to the next multiple of 64 bytes, unless the padded
// envelope would no longer fit; then the plaintext goes out as it is.
func pad(p []byte) []byte {
	n := (len(p) + padBlock - 1) / padBlock * padBlock
	if envelopeLen(n) > MaxEnvelope {
		return p
	}
	return append(p, bytes.Repeat([]byte{' '}, n-len(p))...)
}

// envelopeLen is the length of the envelope for a plaintext of n bytes:
// "pw1." + kid + "." + base64url(nonce || ciphertext || tag).
func envelopeLen(n int) int {
	return len("pw1.") + 8 + 1 + base64.RawURLEncoding.EncodedLen(nonceSize+n+tagSize)
}

// Fit shortens m until it seals into one envelope. Title and subtitle are cut
// to 256 code points and the body to 4096, as the apps would cut them; then
// the body loses code points from its end and, only once it is down to one,
// the title does. Title and body keep at least one code point each and no
// rune is split. Length is measured in the JSON Seal writes, so text that
// escapes counts at its encoded size. Subtitle and url are not shortened any
// further, so a message they alone overfill still does not fit.
func Fit(m Message) Message {
	m.Title = clamp(m.Title, maxTitle)
	m.Subtitle = clamp(m.Subtitle, maxTitle)
	m.Body = clamp(m.Body, maxBody)
	if len(marshal(m)) <= MaxPlaintext {
		return m
	}
	m.Body = keepPrefix(m.Body, func(s string) bool {
		t := m
		t.Body = s
		return len(marshal(t)) <= MaxPlaintext
	})
	m.Title = keepPrefix(m.Title, func(s string) bool {
		t := m
		t.Title = s
		return len(marshal(t)) <= MaxPlaintext
	})
	return m
}

// keepPrefix returns the longest prefix of s, cut at a rune boundary, that
// fits, and at least the first rune when none does. fits must hold for every
// prefix of one it holds for.
func keepPrefix(s string, fits func(string) bool) string {
	if s == "" {
		return s
	}
	// ends[i] is the byte offset just past the (i+1)th rune.
	ends := make([]int, 0, len(s))
	for i := range s {
		if i > 0 {
			ends = append(ends, i)
		}
	}
	ends = append(ends, len(s))
	n := sort.Search(len(ends), func(i int) bool { return !fits(s[:ends[i]]) })
	return s[:ends[max(n, 1)-1]]
}

// SealFit is Seal after Fit: the text is shortened rather than refused. It
// returns ErrTooLong only when the subtitle and url alone leave no room for a
// one-character title and body.
func SealFit(k *Key, m Message) (string, error) {
	return Seal(k, Fit(m))
}

// SealRequest moves req's Title, Subtitle, Body and URL into req.Encrypted,
// sealed under k and shortened as SealFit does, and clears the four fields.
// The rest of the request stays readable (see Message). A request that
// already carries Encrypted is refused, and on any error req is left as it
// was.
func SealRequest(k *Key, req *pushward.SendNotificationRequest) error {
	if req == nil {
		return errors.New("no notification to encrypt")
	}
	if req.Encrypted != "" {
		return errors.New("notification is already encrypted")
	}
	env, err := SealFit(k, Message{Title: req.Title, Subtitle: req.Subtitle, Body: req.Body, URL: req.URL})
	if err != nil {
		return err
	}
	req.Encrypted = env
	req.Title, req.Subtitle, req.Body, req.URL = "", "", "", ""
	return nil
}

var envelopeShape = regexp.MustCompile(`^pw1\.([0-9a-f]{8})\.([A-Za-z0-9_-]{40,})$`)

// ParseEnvelope runs the server's checks on an envelope and returns its Key
// ID and the sealed bytes (nonce, ciphertext, tag). Whether it opens takes
// the key: see Open.
func ParseEnvelope(s string) (kid string, sealed []byte, err error) {
	if s == "" || len(s) > MaxEnvelope {
		return "", nil, fmt.Errorf("an envelope is 1 to %d characters, got %d", MaxEnvelope, len(s))
	}
	m := envelopeShape.FindStringSubmatch(s)
	if m == nil {
		return "", nil, errors.New("not a pw1 envelope (pw1.<key id>.<base64url>)")
	}
	sealed, err = base64.RawURLEncoding.Strict().DecodeString(m[2])
	if err != nil {
		return "", nil, errors.New("envelope is not valid unpadded base64url")
	}
	if len(sealed) < nonceSize+2+tagSize {
		return "", nil, errors.New("envelope is too short")
	}
	return m[1], sealed, nil
}

// Open decrypts an envelope sealed with k and returns the message the way the
// apps show it: title and subtitle cut to 256 code points, the body to 4096,
// and a url the server would refuse dropped. A sender never needs it; it lets
// a test check what a sealed request carries.
func Open(k *Key, envelope string) (Message, error) {
	gcm, err := k.aead()
	if err != nil {
		return Message{}, err
	}
	kid, sealed, err := ParseEnvelope(envelope)
	if err != nil {
		return Message{}, err
	}
	if kid != k.kid {
		return Message{}, fmt.Errorf("sealed with key ID %s, not with this key (%s)", kid, k.kid)
	}
	pt, err := gcm.Open(nil, sealed[:nonceSize], sealed[nonceSize:], []byte("pw1."+kid))
	if err != nil {
		return Message{}, errors.New("cannot decrypt: the envelope was changed or sealed with another key")
	}
	m, err := decode(pt)
	if err != nil {
		return Message{}, err
	}
	m.Title = clamp(m.Title, maxTitle)
	m.Subtitle = clamp(m.Subtitle, maxTitle)
	m.Body = clamp(m.Body, maxBody)
	if validateURL(m.URL) != nil {
		m.URL = ""
	}
	return m, nil
}

// decode reads the plaintext strictly, as the apps do: an object with
// non-empty string title and body, and strings for subtitle and url when
// present. Other keys are ignored.
func decode(pt []byte) (Message, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(pt, &fields); err != nil || fields == nil {
		return Message{}, errors.New("decrypted text is not a JSON object")
	}
	get := func(name string, required bool) (string, error) {
		raw, ok := fields[name]
		if !ok {
			if required {
				return "", fmt.Errorf("decrypted text has no %s", name)
			}
			return "", nil
		}
		var s string
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
			return "", fmt.Errorf("decrypted %s is not a string", name)
		}
		if required && s == "" {
			return "", fmt.Errorf("decrypted %s is empty", name)
		}
		return s, nil
	}
	var m Message
	var err error
	if m.Title, err = get("title", true); err != nil {
		return Message{}, err
	}
	if m.Body, err = get("body", true); err != nil {
		return Message{}, err
	}
	if m.Subtitle, err = get("subtitle", false); err != nil {
		return Message{}, err
	}
	if m.URL, err = get("url", false); err != nil {
		return Message{}, err
	}
	return m, nil
}

// clamp cuts s to n code points.
func clamp(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

func (m Message) validate() error {
	switch {
	case m.Title == "":
		return errors.New("an encrypted notification needs a title")
	case m.Body == "":
		return errors.New("an encrypted notification needs a body")
	case utf8.RuneCountInString(m.Title) > maxTitle:
		return fmt.Errorf("title must not exceed %d characters", maxTitle)
	case utf8.RuneCountInString(m.Subtitle) > maxTitle:
		return fmt.Errorf("subtitle must not exceed %d characters", maxTitle)
	case utf8.RuneCountInString(m.Body) > maxBody:
		return fmt.Errorf("body must not exceed %d characters", maxBody)
	}
	return validateURL(m.URL)
}

// validateURL is the server's url rule. The server cannot see a sealed url,
// so the apps apply it again after opening and drop a url that fails;
// refusing it here tells the sender instead of losing the link silently.
func validateURL(raw string) error {
	if raw == "" {
		return nil
	}
	if utf8.RuneCountInString(raw) > maxURL {
		return fmt.Errorf("url must not exceed %d characters", maxURL)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return errors.New("url must be a valid URL with a scheme")
	}
	switch strings.ToLower(u.Scheme) {
	case "javascript", "data", "file", "vbscript":
		return fmt.Errorf("url uses blocked scheme %q", u.Scheme)
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Host == "" {
		return errors.New("url must include a host for http/https URLs")
	}
	return nil
}
