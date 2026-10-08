package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// vectorsSHA256 pins testdata/vectors-v1.json to the copy every other
// implementation tests against. Update it only together with theirs.
const vectorsSHA256 = "82a670607d831801d5f9054da8d500086c8324146b133d17967966d55493c265"

const testKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

type vectorFile struct {
	Seal []struct {
		Name      string  `json:"name"`
		KeyHex    string  `json:"key_hex"`
		EncKeyHex string  `json:"enc_key_hex"`
		KID       string  `json:"kid"`
		NonceHex  string  `json:"nonce_hex"`
		Plaintext string  `json:"plaintext"`
		Envelope  string  `json:"envelope"`
		Expect    Message `json:"expect"`
	} `json:"seal"`
	OpenFail []struct {
		Name     string `json:"name"`
		KeyHex   string `json:"key_hex"`
		Envelope string `json:"envelope"`
	} `json:"open_fail"`
	ParseFail []struct {
		Name     string `json:"name"`
		Envelope string `json:"envelope"`
	} `json:"parse_fail"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	data, err := os.ReadFile("testdata/vectors-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Seal) == 0 || len(v.OpenFail) == 0 || len(v.ParseFail) == 0 {
		t.Fatal("vector file has an empty section")
	}
	return v
}

func mustKey(t *testing.T, s string) *Key {
	t.Helper()
	k, err := ParseKey(s)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	return k
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVectorsFileIsTheSharedCopy(t *testing.T) {
	data, err := os.ReadFile("testdata/vectors-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != vectorsSHA256 {
		t.Fatalf("vectors-v1.json sha256 = %x, want %s: copy it byte for byte from the other implementations", sum, vectorsSHA256)
	}
}

func TestVectorsSeal(t *testing.T) {
	for _, v := range loadVectors(t).Seal {
		t.Run(v.Name, func(t *testing.T) {
			k := mustKey(t, v.KeyHex)
			if k.KID() != v.KID {
				t.Errorf("kid = %s, want %s", k.KID(), v.KID)
			}
			if got := hex.EncodeToString(k.enc()); got != v.EncKeyHex {
				t.Errorf("enc key = %s, want %s", got, v.EncKeyHex)
			}
			env, err := k.seal(mustHex(t, v.NonceHex), []byte(v.Plaintext))
			if err != nil {
				t.Fatal(err)
			}
			if env != v.Envelope {
				t.Errorf("envelope:\n got %s\nwant %s", env, v.Envelope)
			}
			got, err := Open(k, v.Envelope)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if got != v.Expect {
				t.Errorf("opened %+v, want %+v", got, v.Expect)
			}
		})
	}
}

func TestVectorsOpenFail(t *testing.T) {
	for _, v := range loadVectors(t).OpenFail {
		t.Run(v.Name, func(t *testing.T) {
			if m, err := Open(mustKey(t, v.KeyHex), v.Envelope); err == nil {
				t.Errorf("opened to %+v, want a failure", m)
			}
		})
	}
}

func TestVectorsParseFail(t *testing.T) {
	for _, v := range loadVectors(t).ParseFail {
		t.Run(v.Name, func(t *testing.T) {
			if _, _, err := ParseEnvelope(v.Envelope); err == nil {
				t.Error("parsed, want a failure")
			}
		})
	}
}

// Seal itself (validation, JSON, padding, nonce) reproduces the two vectors
// whose plaintext is exactly what a sender writes: the padded one and the
// unpadded maximum.
func TestSealReproducesSenderVectors(t *testing.T) {
	for _, v := range loadVectors(t).Seal {
		if v.Name != "padded_to_64" && v.Name != "max_size" {
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			env, err := sealMessage(mustKey(t, v.KeyHex), v.Expect, bytes.NewReader(mustHex(t, v.NonceHex)))
			if err != nil {
				t.Fatal(err)
			}
			if env != v.Envelope {
				t.Errorf("envelope:\n got %s\nwant %s", env, v.Envelope)
			}
		})
	}
}

func TestSealRoundTrips(t *testing.T) {
	k := mustKey(t, testKey)
	for _, m := range []Message{
		{Title: "Disk full", Body: "/var is at 97% on db01"},
		{Title: "Deploy failed", Subtitle: "api / production", Body: "Step 3 of 5: migrate", URL: "https://ci.example.com/runs/42"},
		{Title: "<b>&</b>", Body: "html stays as typed", URL: "myapp://incident/7"},
		{Title: strings.Repeat("\U0001F525", 100), Body: strings.Repeat("\u05ea", 500)},
	} {
		env, err := Seal(k, m)
		if err != nil {
			t.Fatalf("Seal(%+v): %v", m, err)
		}
		if got, err := Open(k, env); err != nil || got != m {
			t.Errorf("round trip = %+v, %v; want %+v", got, err, m)
		}
	}
	a, _ := Seal(k, Message{Title: "t", Body: "b"})
	b, _ := Seal(k, Message{Title: "t", Body: "b"})
	if a == b {
		t.Error("two seals of one message share a nonce")
	}
}

// Every plaintext is padded to a multiple of 64 bytes unless the padded
// envelope would be over MaxEnvelope; then it goes out as it is.
func TestSealPadding(t *testing.T) {
	k := mustKey(t, testKey)
	for n := 1; ; n++ {
		m := Message{Title: "t", Body: strings.Repeat("x", n)}
		plain := len(marshal(m))
		if plain > MaxPlaintext {
			break
		}
		env, err := Seal(k, m)
		if err != nil {
			t.Fatalf("%d bytes: %v", plain, err)
		}
		_, raw, err := ParseEnvelope(env)
		if err != nil {
			t.Fatalf("%d bytes: sealed envelope does not parse: %v", plain, err)
		}
		want := (plain + padBlock - 1) / padBlock * padBlock
		if want > MaxPlaintext {
			want = plain
		}
		if got := len(raw) - nonceSize - tagSize; got != want {
			t.Fatalf("%d bytes of JSON sealed as %d, want %d", plain, got, want)
		}
		if len(env) > MaxEnvelope {
			t.Fatalf("%d bytes of JSON: envelope is %d characters", plain, len(env))
		}
	}
}

func TestSealRefuses(t *testing.T) {
	k := mustKey(t, testKey)
	for name, m := range map[string]Message{
		"no title":       {Body: "b"},
		"no body":        {Title: "t"},
		"long title":     {Title: strings.Repeat("a", maxTitle+1), Body: "b"},
		"long subtitle":  {Title: "t", Subtitle: strings.Repeat("a", maxTitle+1), Body: "b"},
		"long body":      {Title: "t", Body: strings.Repeat("a", maxBody+1)},
		"javascript url": {Title: "t", Body: "b", URL: "javascript:alert(1)"},
		"data url":       {Title: "t", Body: "b", URL: "DATA:text/html,x"},
		"http no host":   {Title: "t", Body: "b", URL: "https:///path"},
		"no scheme":      {Title: "t", Body: "b", URL: "example.com/x"},
		"long url":       {Title: "t", Body: "b", URL: "https://example.com/" + strings.Repeat("a", maxURL)},
	} {
		if env, err := Seal(k, m); err == nil {
			t.Errorf("%s: sealed to %s, want an error", name, env)
		}
	}
	// One byte over the max_size vector.
	if _, err := Seal(k, Message{Title: "Max", Body: strings.Repeat("x", 2242)}); !errors.Is(err, ErrTooLong) {
		t.Errorf("one byte over: err = %v, want ErrTooLong", err)
	}
	if _, err := Seal(k, Message{Title: "Max", Body: strings.Repeat("x", 2241)}); err != nil {
		t.Errorf("largest plaintext: %v", err)
	}
	if _, err := sealMessage(k, Message{Title: "t", Body: "b"}, bytes.NewReader([]byte{1, 2, 3})); err == nil {
		t.Error("a short nonce read must fail")
	}
	for _, key := range []*Key{nil, {}} {
		if _, err := Seal(key, Message{Title: "t", Body: "b"}); err == nil {
			t.Errorf("Seal with %#v: want an error", key)
		}
		if _, err := Open(key, "pw1.767c0806.AAAA"); err == nil {
			t.Errorf("Open with %#v: want an error", key)
		}
	}
}

func TestParseKey(t *testing.T) {
	const want = "767c0806"
	for _, s := range []string{
		testKey,
		strings.ToUpper(testKey),
		" 00010203 04050607 08090a0b 0c0d0e0f\n10111213 14151617\t18191a1b 1c1d1e1f\r\n",
	} {
		if k := mustKey(t, s); k.KID() != want {
			t.Errorf("ParseKey(%q).KID() = %s, want %s", s, k.KID(), want)
		}
	}
	for _, s := range []string{
		"",
		testKey[:62],
		testKey + "00",
		"zz" + testKey[2:],
		"\u00e9" + testKey[2:],
	} {
		_, err := ParseKey(s)
		if err == nil {
			t.Errorf("ParseKey(%q) accepted", s)
			continue
		}
		if len(s) > 8 && strings.Contains(err.Error(), s[2:10]) {
			t.Errorf("ParseKey error repeats the input: %v", err)
		}
	}
	for _, s := range []string{"hlk_0123456789abcdef0123456789abcdef", "  HLA_0123456789abcdef\n", "Hlk_x"} {
		_, err := ParseKey(s)
		if !errors.Is(err, ErrIntegrationKey) {
			t.Errorf("ParseKey(%q): err = %v, want ErrIntegrationKey", s, err)
		}
		if err != nil && strings.Contains(err.Error(), "0123456789") {
			t.Errorf("ParseKey error repeats the input: %v", err)
		}
	}
}

// No formatting path may print the key or the AES key derived from it, not
// even a Key held in an unexported field, where fmt cannot call String.
func TestKeyIsNeverPrinted(t *testing.T) {
	k := mustKey(t, testKey)
	raw := mustHex(t, testKey)
	secrets := []string{
		testKey, strings.ToUpper(testKey), fmt.Sprintf("%d", raw),
		hex.EncodeToString(k.enc()), strings.ToUpper(hex.EncodeToString(k.enc())), fmt.Sprintf("%d", k.enc()),
	}
	check := func(where, out string) {
		t.Helper()
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("%s prints key material: %s", where, out)
				return
			}
		}
	}

	type holder struct {
		key Key
		ptr *Key
		Key Key
		Ptr *Key
	}
	h := holder{*k, k, *k, k}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		for name, v := range map[string]any{"*Key": k, "Key": *k, "struct": h, "*struct": &h, "slice": []*Key{k}} {
			check(verb+" of "+name, fmt.Sprintf(verb, v))
		}
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if got := fmt.Sprintf(verb, k); got != "e2e key 767c0806" {
			t.Errorf("%s = %q, want %q", verb, got, "e2e key 767c0806")
		}
	}

	var buf bytes.Buffer
	for _, hd := range []slog.Handler{slog.NewJSONHandler(&buf, nil), slog.NewTextHandler(&buf, nil)} {
		slog.New(hd).Info("sending", "key", k, "value", *k, "holder", h, slog.Group("g", "key", k))
	}
	check("slog", buf.String())
	if !strings.Contains(buf.String(), "e2e key 767c0806") {
		t.Errorf("slog output does not name the key by its Key ID: %s", buf.String())
	}

	j, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	check("json", string(j))
}

// units are the shapes of text whose JSON size differs from its UTF-8 size,
// plus plain ASCII and invalid UTF-8.
var units = []string{
	"x",
	"\u00e9",     // 2 bytes
	"\u6f22",     // CJK, 3 bytes
	"\U0001F525", // emoji, 4 bytes
	`"\`,         // 2 bytes each once escaped
	"\x01",       // \u0001, 6 bytes
	"\u2028",     // escaped by Go, 6 bytes
	"<&>",        // not escaped: SetEscapeHTML(false)
	"\xff",       // invalid UTF-8, U+FFFD once encoded
	"a\u00e9\u6f22\U0001F525\"\x01\u2028 ",
}

func TestFitSweep(t *testing.T) {
	k := mustKey(t, testKey)
	for _, unit := range units {
		for _, n := range []int{1, 50, 300, 1000, 2266, 5000} {
			for _, title := range []string{"Disk full", strings.Repeat(unit, 300)} {
				for _, sub := range []string{"", "api / production"} {
					for _, link := range []string{"", "https://ci.example.com/runs/42"} {
						m := Message{Title: title, Subtitle: sub, Body: strings.Repeat(unit, n), URL: link}
						checkFit(t, k, m)
					}
				}
			}
		}
	}
}

func checkFit(t *testing.T, k *Key, m Message) {
	t.Helper()
	name := fmt.Sprintf("title %d bytes, body %d bytes of %q", len(m.Title), len(m.Body), m.Body[:min(len(m.Body), 12)])
	f := Fit(m)
	title, body := clamp(m.Title, maxTitle), clamp(m.Body, maxBody)
	if f.Title == "" || !strings.HasPrefix(title, f.Title) || f.Body == "" || !strings.HasPrefix(body, f.Body) {
		t.Fatalf("%s: Fit gave title %q, body %q: not non-empty prefixes", name, f.Title, f.Body)
	}
	if f.Subtitle != m.Subtitle || f.URL != m.URL {
		t.Fatalf("%s: Fit changed subtitle or url", name)
	}
	if len(marshal(f)) > MaxPlaintext {
		t.Fatalf("%s: Fit left %d bytes of JSON", name, len(marshal(f)))
	}
	// Tight: one more code point of the body would not have fit, and the
	// title was cut only with the body down to one.
	if f.Body != body {
		_, size := utf8.DecodeRuneInString(body[len(f.Body):])
		more := f
		more.Body = body[:len(f.Body)+size]
		if len(marshal(more)) <= MaxPlaintext {
			t.Fatalf("%s: body cut to %d bytes, but %d bytes fit", name, len(f.Body), len(more.Body))
		}
	}
	if f.Title != title && utf8.RuneCountInString(f.Body) != 1 {
		t.Fatalf("%s: title cut while the body kept %d code points", name, utf8.RuneCountInString(f.Body))
	}

	env, err := SealFit(k, m)
	if err != nil {
		t.Fatalf("%s: SealFit: %v", name, err)
	}
	if len(env) > MaxEnvelope {
		t.Fatalf("%s: envelope is %d characters", name, len(env))
	}
	got, err := Open(k, env)
	if err != nil {
		t.Fatalf("%s: Open: %v", name, err)
	}
	for _, s := range []string{got.Title, got.Subtitle, got.Body, got.URL} {
		if !utf8.ValidString(s) {
			t.Fatalf("%s: opened text is not valid UTF-8: %q", name, s)
		}
	}
	if utf8.ValidString(m.Title) && utf8.ValidString(m.Body) && (got.Title != f.Title || got.Body != f.Body) {
		t.Fatalf("%s: opened %q / %q, want %q / %q", name, got.Title, got.Body, f.Title, f.Body)
	}
}

func TestFitTrimsBodyBeforeTitle(t *testing.T) {
	m := Fit(Message{Title: strings.Repeat("T", 300), Body: strings.Repeat("b", 5000)})
	if m.Title != strings.Repeat("T", maxTitle) {
		t.Errorf("title cut to %d characters while the body had room to give", len(m.Title))
	}
	if n := len(marshal(m)); n != MaxPlaintext {
		t.Errorf("ASCII body should fill the envelope exactly: %d bytes of JSON", n)
	}

	// A subtitle that leaves less room than the title needs: the body goes
	// down to one code point first, then the title is cut.
	m = Fit(Message{
		Title:    strings.Repeat("\u2028", maxTitle),
		Subtitle: strings.Repeat("\u2028", maxTitle),
		Body:     "body",
	})
	if m.Body != "b" {
		t.Errorf("body = %q, want it down to one code point", m.Body)
	}
	if n := utf8.RuneCountInString(m.Title); n == 0 || n == maxTitle {
		t.Errorf("title kept %d code points, want it cut", n)
	}
	if len(marshal(m)) > MaxPlaintext {
		t.Errorf("does not fit: %d bytes of JSON", len(marshal(m)))
	}

	// A message that fits comes back as it was.
	in := Message{Title: "Disk full", Subtitle: "nas", Body: "/var is at 97%", URL: "https://nas.example.com"}
	if got := Fit(in); got != in {
		t.Errorf("Fit changed a message that fits: %+v", got)
	}
}

func TestSealFitTooLong(t *testing.T) {
	k := mustKey(t, testKey)
	m := Message{
		Title:    "t",
		Subtitle: strings.Repeat("\u2028", maxTitle),
		Body:     "b",
		URL:      "https://example.com/" + strings.Repeat("a", maxURL-len("https://example.com/")),
	}
	if _, err := SealFit(k, m); !errors.Is(err, ErrTooLong) {
		t.Errorf("err = %v, want ErrTooLong", err)
	}
	m.Subtitle = ""
	if _, err := SealFit(k, m); err != nil {
		t.Errorf("without the subtitle: %v", err)
	}
}

func TestSealRequest(t *testing.T) {
	k := mustKey(t, testKey)
	req := pushward.SendNotificationRequest{
		Title:       "Disk full",
		Subtitle:    "nas",
		Body:        "/var is at 97%",
		URL:         "https://nas.example.com/storage",
		Level:       pushward.LevelActive,
		CollapseID:  "nas-disk",
		Source:      "grafana",
		Metadata:    map[string]string{"severity": "critical"},
		Acknowledge: &pushward.NotificationAcknowledge{RepeatSeconds: 300},
		Tags:        []string{"nas-disk"},
	}
	want := req
	want.Title, want.Subtitle, want.Body, want.URL = "", "", "", ""

	if err := SealRequest(k, &req); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(req.Encrypted, "pw1.767c0806.") {
		t.Fatalf("encrypted = %q", req.Encrypted)
	}
	got, err := Open(k, req.Encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Message{Title: "Disk full", Subtitle: "nas", Body: "/var is at 97%", URL: "https://nas.example.com/storage"}) {
		t.Errorf("opened %+v", got)
	}
	want.Encrypted = req.Encrypted
	if !reflect.DeepEqual(req, want) {
		t.Errorf("request after sealing:\n got %+v\nwant %+v", req, want)
	}

	// Already encrypted, unsealable or no key: refused and left as it was.
	for name, tc := range map[string]struct {
		key *Key
		req pushward.SendNotificationRequest
	}{
		"already encrypted": {k, pushward.SendNotificationRequest{Encrypted: req.Encrypted}},
		"no body":           {k, pushward.SendNotificationRequest{Title: "t"}},
		"blocked url":       {k, pushward.SendNotificationRequest{Title: "t", Body: "b", URL: "javascript:alert(1)"}},
		"no key":            {nil, pushward.SendNotificationRequest{Title: "t", Body: "b"}},
	} {
		before := tc.req
		if err := SealRequest(tc.key, &tc.req); err == nil {
			t.Errorf("%s: sealed, want an error", name)
		}
		if !reflect.DeepEqual(tc.req, before) {
			t.Errorf("%s: request changed on error: %+v", name, tc.req)
		}
	}

	if err := SealRequest(k, nil); err == nil {
		t.Error("SealRequest(k, nil): want an error")
	}
	if err := SealRequest(nil, nil); err == nil {
		t.Error("SealRequest(nil, nil): want an error")
	}

	// Too long for one envelope: the body is shortened, not refused.
	long := pushward.SendNotificationRequest{Title: "Log", Body: strings.Repeat("line\n", 900)}
	if err := SealRequest(k, &long); err != nil {
		t.Fatal(err)
	}
	if got, err := Open(k, long.Encrypted); err != nil || !strings.HasPrefix(strings.Repeat("line\n", 900), got.Body) {
		t.Errorf("opened %q, %v", got.Body, err)
	}
}

// A sealed request is what the server accepts: no readable text next to the
// envelope.
func TestSealedRequestPassesTheMock(t *testing.T) {
	k := mustKey(t, testKey)
	srv, calls, mu := testutil.MockPushWardServer(t)
	req := pushward.SendNotificationRequest{Title: "Disk full", Body: "/var is at 97%", Source: "grafana"}
	if err := SealRequest(k, &req); err != nil {
		t.Fatal(err)
	}
	if err := pushward.NewClient(srv.URL, "hlk_test").SendNotification(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	testutil.UnmarshalBody(t, testutil.GetCalls(calls, mu)[0].Body, &sent)
	if sent["title"] != "" || sent["body"] != "" || sent["source"] != "grafana" {
		t.Errorf("sent %v", sent)
	}
	if m, err := Open(k, sent["encrypted"].(string)); err != nil || m.Title != "Disk full" {
		t.Errorf("opened %+v, %v", m, err)
	}
}
