package universal

import (
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Made-up values, none taken from a real payload. The GitHub token is split
// so secret scanners do not take the test for a leak.
const (
	testJWT      = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.c2lnbmF0dXJlLW5vdC1yZWFs"
	testGitHub   = "ghp_" + "R2d2C3poL8kQ7xZ4vN1mW9sT6yB0aJ5hG3fE"
	testRandom   = "q7Xk2Vb9Rt4Nm8Zc3Wp6Ly1H"
	testStripeID = "ch_3Tq9Wx7Kd2Lm8Zp4Rv6Nb1Y"
	testHex32    = "0123456789abcdef0123456789abcdef"
	testUUID     = "0f8e7d6c-5b4a-4321-8fed-cba987654321"
)

func TestClassOf(t *testing.T) {
	cases := []struct {
		path, value string
		typ         ValueType
		want        ValueClass
	}{
		{"title", "Disk full on web-1", TypeString, ClassText},
		{"status", "firing", TypeString, ClassEnum},
		{"author", "Anna", TypeString, ClassEnum},
		{"authorizationStatus", "CANCELED", TypeString, ClassEnum},
		{"session_name", "my_session", TypeString, ClassEnum},
		{"labels.value", "prod", TypeString, ClassEnum},
		{"sort_key", "name", TypeString, ClassEnum},
		{"links.html", "https://example.com/a?b=c", TypeString, ClassURL},
		{"label_url", "https://s3.example.com/l.pdf?AWSAccessKeyId=AKIAEXAMPLE0KEY00001&Signature=x", TypeString, ClassURL},
		{"id", testUUID, TypeString, ClassID},
		{"sha", testHex32 + "01234567", TypeString, ClassID},
		{"number", "123", TypeString, ClassID},
		{"session_id", testUUID, TypeString, ClassID},
		{"charge_id", testStripeID, TypeString, ClassID},
		{"user_uid", testRandom, TypeString, ClassID},
		{"payment_ref", testRandom, TypeString, ClassID},
		{"pipelineKey", testRandom, TypeString, ClassID},
		{"count", "3", TypeNumber, ClassNumber},
		{"severity", "3", TypeString, ClassNumber},
		{"level", "42", TypeString, ClassNumber},
		{"vehicleTokenId", "17", TypeNumber, ClassNumber},
		{"ok", "true", TypeBool, ClassBool},
		{"fired_at", "2026-09-26T10:00:00Z", TypeString, ClassTime},
		{"user", "anna@example.com", TypeString, ClassEmail},
		{"note", "", TypeNull, ClassEmpty},
		{"tags[]", "", TypeEmpty, ClassEmpty},
		{"note", "   ", TypeString, ClassEmpty},
		{"label", "sk-learn", TypeString, ClassEnum},
		// Too short for a generated token, and nothing in the key says secret.
		{"data", "a7Kq2Zx9Lm4Pw8Rt3Vy", TypeString, ClassEnum},

		{"access_token", "abc", TypeString, ClassSecret},
		{"headers.X-Api-Key", "abc", TypeString, ClassSecret},
		{"X-Hub-Signature-256", "sha256=abc", TypeString, ClassSecret},
		{"privateKey", "abc", TypeString, ClassSecret},
		{"accesstoken", "abc", TypeString, ClassSecret},
		{"password", "", TypeNull, ClassSecret},
		{"cookies[]", "a=b", TypeString, ClassSecret},
		{"session", "a", TypeString, ClassSecret},
		{"is_private", "true", TypeBool, ClassSecret},
		{"pin", "1234", TypeString, ClassSecret},
		{"otp", "123456", TypeString, ClassSecret},
		{"passcode", "0000", TypeString, ClassSecret},
		{"auth.pass", "hunter2", TypeString, ClassSecret},
		{"token.value", "abc", TypeString, ClassSecret},
		{"token.data", "a7Kq2Zx9Lm4Pw8Rt3Vy", TypeString, ClassSecret},
		{"secrets[].value", "abc", TypeString, ClassSecret},
		{"apiKey.key", "abc", TypeString, ClassSecret},
		{"hmac_key", "abc", TypeString, ClassSecret},
		{"license_key", "abc", TypeString, ClassSecret},
		{"webhookKey", "abc", TypeString, ClassSecret},
		{"ssh_keys[]", "abc", TypeString, ClassSecret},
		{"note", testJWT, TypeString, ClassSecret},
		{"header", "Bearer " + testJWT, TypeString, ClassSecret},
		{"value", testGitHub, TypeString, ClassSecret},
		{"value", "token=" + testGitHub, TypeString, ClassSecret},
		{"value", "hlk_abcd1234", TypeString, ClassSecret},
		{"value", "AKIAEXAMPLE0KEY00001", TypeString, ClassSecret},
		{"value", "ASIAEXAMPLE0KEY00002", TypeString, ClassSecret},
		{"mailgun", "key-" + testHex32, TypeString, ClassSecret},
		{"signing", "whsec_Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MA==", TypeString, ClassSecret},
		{"twilio", "SK" + testHex32, TypeString, ClassSecret},
		{"cert", "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", TypeString, ClassSecret},
		{"data", testRandom, TypeString, ClassSecret},
		{"reference_code", testStripeID, TypeString, ClassSecret},
		{"note", "https://x.example.com/a failed", TypeString, ClassText},

		// Key words, their forms and their parents.
		{"token_value", "a", TypeString, ClassSecret},
		{"password_confirm", "a", TypeString, ClassSecret},
		{"passwordConfirmation", "a", TypeString, ClassSecret},
		{"api_key_b64", "a", TypeString, ClassSecret},
		{"secret_v2", "a", TypeString, ClassSecret},
		{"Authorization-Header", "a", TypeString, ClassSecret},
		{"new_password", "a", TypeString, ClassSecret},
		{"password_old", "a", TypeString, ClassSecret},
		{"api.key", "a", TypeString, ClassSecret},
		{"encryption.key", "a", TypeString, ClassSecret},
		{"hmac.key", "a", TypeString, ClassSecret},
		{"webhook.key", "a", TypeString, ClassSecret},
		{"cookies.session_id", "a", TypeString, ClassSecret},
		{"secrets.github", "a", TypeString, ClassSecret},
		{"credentials.aws.username", "a", TypeString, ClassSecret},
		{"creds", "a", TypeString, ClassSecret},
		{"jwt", "a", TypeString, ClassSecret},
		{"bearer", "a", TypeString, ClassSecret},
		{"sentry_dsn", "a", TypeString, ClassSecret},
		{"DATABASE_URL", "a", TypeString, ClassSecret},
		{"connection_string", "a", TypeString, ClassSecret},
		{"connectionString", "a", TypeString, ClassSecret},
		{"mongo_uri", "a", TypeString, ClassSecret},
		{"mnemonic", "a", TypeString, ClassSecret},
		{"seed", "a", TypeString, ClassSecret},
		{"privkey", "a", TypeString, ClassSecret},
		{"pat", "a", TypeString, ClassSecret},
		{"backup_code", "a", TypeString, ClassSecret},
		{"recovery_codes[]", "a", TypeString, ClassSecret},
		{"sig", "a", TypeString, ClassSecret},
		{"hmac", "a", TypeString, ClassSecret},
		{"sort.key", "name", TypeString, ClassEnum},
		{"pattern", "name", TypeString, ClassEnum},
		{"database", "name", TypeString, ClassEnum},

		// Credential formats.
		{"v", "AIza" + strings.Repeat("Ab1_", 8) + "Ab1", TypeString, ClassSecret},
		{"v", "SG." + strings.Repeat("a", 22) + "." + strings.Repeat("b", 43), TypeString, ClassSecret},
		{"v", "npm_" + strings.Repeat("a1", 18), TypeString, ClassSecret},
		{"v", "lin_api_" + strings.Repeat("a1", 20), TypeString, ClassSecret},
		{"v", "shpat_" + testHex32, TypeString, ClassSecret},
		{"v", "glrt-" + strings.Repeat("a1", 10), TypeString, ClassSecret},
		{"v", "x_sk_live_" + strings.Repeat("a1", 6), TypeString, ClassSecret},
	}
	for _, c := range cases {
		if got := ClassOf(c.path, Field{Path: c.path, Value: c.value, Type: c.typ}); got != c.want {
			t.Errorf("ClassOf(%s = %q) = %s, want %s", c.path, c.value, got, c.want)
		}
	}
}

func TestDisplay(t *testing.T) {
	cases := []struct {
		path, value string
		typ         ValueType
		want        string
	}{
		{"token", "abc", TypeString, "[redacted]"},
		{"user", "anna@example.com", TypeString, "[email]"},
		{"status", "firing", TypeString, "firing"},
		{"count", "42", TypeNumber, "42"},
		{"ok", "false", TypeBool, "false"},
		{"note", "", TypeNull, ""},
		{"data", "a7Kq2Zx9Lm4Pw8Rt3Vy", TypeString, "a7Kq2Zx9Lm4Pw8Rt3Vy"},

		{"url", "https://bot:hunter2@grafana.example.com/d/abc?orgId=1&token=x#panel", TypeString, "https://grafana.example.com/d/abc"},
		{"url", "https://hooks.slack.com/services/T0A1B2C3D/B0A1B2C3D/" + testRandom, TypeString, "https://hooks.slack.com/services/T0A1B2C3D/B0A1B2C3D/..."},
		{"url", "https://example.com/u/anna%40example.com/edit", TypeString, "https://example.com/u/.../edit"},
		{"url", "https://example.com/hooks/x-" + testGitHub, TypeString, "https://example.com/hooks/..."},
		{"url", "https://" + testRandom + ".tunnel.example.com/a", TypeString, "https://....tunnel.example.com/a"},
		{"url", "https://api.telegram.org/bot123456789:AAH" + testRandom + "/sendMessage", TypeString, "https://api.telegram.org/.../sendMessage"},
		{"url", "https://hc-ping.example.com/" + testUUID, TypeString, "https://hc-ping.example.com/..."},
		{"url", "https://ha.example.com/api/webhook/" + testHex32 + testHex32, TypeString, "https://ha.example.com/api/webhook/..."},
		{"url", "https://shop.example.com/cart;jsessionid=0A1B2C3D4E5F?x=1", TypeString, "https://shop.example.com/cart"},
		{"url", "https://ex%zz.example.com/", TypeString, "[url]"},

		{"message", "Deploy failed: key " + testGitHub + " revoked by (anna@example.com).", TypeString, "Deploy failed: key [redacted] revoked by ([email])."},
		{"message", "Run " + testRandom + " finished in 3 minutes", TypeString, "Run [redacted] finished in 3 minutes"},
		{"message", "at 2026-09-26T10:00 token " + testGitHub, TypeString, "at 2026-09-26T10:00 token [redacted]"},
		{"message", "see https://bob:hunter2@ci.example.com/job/1?token=abcd1234#x now", TypeString, "see https://ci.example.com/job/1 now"},
		{"message", "header was Bearer abcdefgh12345678 today", TypeString, "header was Bearer [redacted] today"},
		{"message", "The token was revoked; the Basic plan ends", TypeString, "The token was revoked; the Basic plan ends"},
		{"message", "login failed, password=hunter2 user=anna", TypeString, "login failed, password=[redacted] user=anna"},
		{"message", `payload {"api_key": "abc123", "user": "anna"}`, TypeString, `payload {"api_key": [redacted], "user": "anna"}`},
		{"message", "Paid with 4111 1111 1111 1111 today", TypeString, "Paid with [redacted] today"},
		{"message", "Order 4111 1111 1111 1112 today", TypeString, "Order 4111 1111 1111 1112 today"},
		{"message", "Refund to DE89 3704 0044 0532 0130 00 soon", TypeString, "Refund to [redacted] soon"},
		{"message", "Call +48 123 456 789 now", TypeString, "Call [redacted] now"},
		{"message", "secret " + testHex32 + " set", TypeString, "secret [redacted] set"},
		{"note", testHex32 + testHex32, TypeString, "[redacted]"},
		{"commit_sha", testHex32 + "01234567", TypeString, testHex32 + "01234567"},
		{"label", "prod-" + testHex32, TypeString, "prod-[redacted]"},
		{"key", testHex32, TypeString, "[redacted]"},

		// Text around URLs.
		{"message", "https://ci.example.com/job/1 failed: password=hunter2", TypeString, "https://ci.example.com/job/1 failed: password=[redacted]"},
		{"message", "https://shop.example.com/o/1 paid 4111 1111 1111 1111", TypeString, "https://shop.example.com/o/1 paid [redacted]"},
		{"message", "https://bank.example.com/t DE89 3704 0044 0532 0130 00", TypeString, "https://bank.example.com/t [redacted]"},
		{"message", "https://x.example.com/a " + testHex32, TypeString, "https://x.example.com/a [redacted]"},
		{"message", `{"url":"https://x.example.com/cb","password":"hunter2"}`, TypeString, `{"url":"https://x.example.com/cb","password":[redacted]}`},
		{"url", "https://api.example.com/api/v1/token=abcdef1234", TypeString, "https://api.example.com/api/v1/..."},
		{"url", "https://api.example.com/hooks/wh_" + testHex32, TypeString, "https://api.example.com/hooks/..."},
		{"url", "https://" + testHex32[:16] + ".example.com/a", TypeString, "https://....example.com/a"},
		{"url", "https://maker.example.com/with/key/dQw4w9WgXcQzR8kPlM2vbN", TypeString, "https://maker.example.com/with/key/..."},
		{"url", "https://example.com/MyProjectReport/2024", TypeString, "https://example.com/MyProjectReport/2024"},

		// Userinfo in any scheme.
		{"message", "db at postgres://app:hunter2@db:5432/app is down", TypeString, "db at postgres://[redacted]@db:5432/app is down"},
		{"message", "cache redis://:hunter2@cache:6379 gone", TypeString, "cache redis://[redacted]@cache:6379 gone"},
		{"message", "queue amqp://guest:guest@mq/vhost and ftp://u:p@files/x", TypeString, "queue amqp://[redacted]@mq/vhost and ftp://[redacted]@files/x"},

		// key=value forms.
		{"message", "login pass=abc pin: 1234 otp=123456", TypeString, "login pass=[redacted] pin: [redacted] otp=[redacted]"},
		{"message", "set passphrase: correct horse", TypeString, "set passphrase: [redacted] horse"},
		{"message", "credentials=abc private_key=def sig=ghi", TypeString, "credentials=[redacted] private_key=[redacted] sig=[redacted]"},
		{"message", `opts :password => "hunter2" done`, TypeString, `opts :password => [redacted] done`},
		{"message", `body {\"password\":\"hunter2\"} sent`, TypeString, `body {\"password\":[redacted]} sent`},
		{"message", "q password=a,b;c&d next", TypeString, "q password=[redacted] next"},
		{"message", "the compass: north and bypass: on", TypeString, "the compass: north and bypass: on"},

		// Personal data.
		{"message", "call (415) 555-1234 or 415-555-1234", TypeString, "call [redacted] or [redacted]"},
		{"message", "ssn 123-45-6789 on file", TypeString, "ssn [redacted] on file"},
		{"message", "to de89 3704 0044 0532 0130 00 soon", TypeString, "to [redacted] soon"},
		{"message", "mail anna@ex\u00e4mple.de now", TypeString, "mail [email] now"},
		{"user", "anna@ex\u00e4mple.de", TypeString, "[email]"},
	}
	for _, c := range cases {
		if got := Display(c.path, Field{Path: c.path, Value: c.value, Type: c.typ}, 120); got != c.want {
			t.Errorf("Display(%s = %q) =\n%q\nwant\n%q", c.path, c.value, got, c.want)
		}
	}
	long := Field{Path: "message", Value: strings.Repeat("word ", 20), Type: TypeString}
	if got := Display(long.Path, long, 20); got != "word word word wo..." {
		t.Errorf("long text = %q, want it cut to 20 runes", got)
	}
	num := Field{Path: "n", Value: "12345678901234567890", Type: TypeNumber}
	if got := Display(num.Path, num, 10); got != "1234567..." {
		t.Errorf("long number = %q, want it cut to 10 runes", got)
	}
}

var userinfoPassword = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^/?#\s@]*:[^/?#\s@]*@`)

// Hook keys generated at random must not survive Display. The rules cannot
// catch every draw (a key without digits and with many vowels reads as
// words), but they must catch nearly all.
func TestDisplayHookKeys(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) // #nosec G404 -- a fixed seed, so the rate is stable
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	draw := func(alphabet string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[rng.IntN(len(alphabet))]
		}
		return string(b)
	}
	upper := alnum[:26] + alnum[52:]
	cases := map[string]func() (string, string){
		"slack": func() (string, string) {
			key := draw(alnum, 24)
			return "https://hooks.slack.com/services/T" + draw(upper, 8) + "/B" + draw(upper, 10) + "/" + key, key
		},
		"ifttt": func() (string, string) {
			key := draw(alnum+"-_", 22)
			return "https://maker.ifttt.com/trigger/door/with/key/" + key, key
		},
	}
	for name, gen := range cases {
		const n = 2000
		missed := 0
		for range n {
			u, key := gen()
			if strings.Contains(Display("url", Field{Path: "url", Value: u, Type: TypeString}, 200), key) {
				missed++
			}
		}
		if rate := float64(missed) / n; rate >= 0.03 {
			t.Errorf("%s: %d of %d keys shown (%.1f%%), want under 3%%", name, missed, n, 100*rate)
		} else {
			t.Logf("%s: %d of %d keys shown", name, missed, n)
		}
	}
}

func FuzzDisplay(f *testing.F) {
	f.Add("token", "abc")
	f.Add("note", "Bearer "+testJWT)
	f.Add("link", "https://user:pass@example.com/a/"+testRandom+"?token=x#frag")
	f.Add("message", "key "+testGitHub+" leaked, mail anna@example.com")
	f.Add("message", "2026-09-26T10:00 token "+testGitHub)
	f.Add("message", "see https://bob:hunter2@ci.example.com/job/1?token=abcd1234#x")
	f.Add("message", "hlk_https://x.example.com/a;jsessionid=1?b")
	f.Add("data", testRandom)
	f.Add("note", "AKIA0000000000000000eyJ0000000000 and more")
	f.Add("count", "42")
	f.Add("message", `{"url":"https://x.example.com/cb","password":"hunter2"}`)
	f.Add("message", "postgres://app:hunter2@db:5432/app and 4111 1111 1111 1111")
	f.Add("message", "password=https://a.example.com/x")
	f.Add("0", "http://0000000000000")
	f.Add("0", "http://000000000 0000#")
	f.Add("0", "http://A://:@")
	f.Fuzz(func(t *testing.T, path, value string) {
		for _, typ := range []ValueType{TypeString, TypeNumber, TypeBool, TypeNull} {
			fld := Field{Path: path, Value: capRunes(value, MaxValueRunes), Type: typ}
			// Flatten only makes numbers and bools from JSON literals.
			if _, err := strconv.ParseFloat(fld.Value, 64); typ == TypeNumber && err != nil {
				continue
			}
			if typ == TypeBool && fld.Value != "true" && fld.Value != "false" {
				continue
			}
			out := Display(path, fld, 64)
			v := strings.TrimSpace(fld.Value)
			switch ClassOf(path, fld) {
			case ClassSecret:
				if out != redacted || (v != "" && !strings.Contains(redacted, v) && strings.Contains(out, v)) {
					t.Fatalf("secret %q shown as %q", v, out)
				}
			case ClassURL:
				if strings.ContainsAny(out, "?#") {
					t.Fatalf("URL %q shown with its query or fragment: %q", v, out)
				}
			}
			if w := credentialIn.FindString(out); w != "" {
				t.Fatalf("credential %q shown in %q", w, out)
			}
			// URLs inside text are shown as displayURL shows them. The output
			// itself can end a URL in "#": text after it that was masked.
			for _, u := range urlIn.FindAllString(v, -1) {
				if d := displayURL(u); strings.ContainsAny(d, "?#") {
					t.Fatalf("URL %q inside %q shown as %q", u, v, d)
				}
			}
			// Uncut, so a placeholder cut in half does not read as a value.
			full := Display(path, fld, 4*MaxValueRunes)
			for _, m := range keyValue.FindAllStringSubmatch(full, -1) {
				if unmasked(m[1]) != 0 {
					t.Fatalf("value %q of %q shown in %q", m[1], m[0], full)
				}
			}
			for _, c := range cardIn.FindAllString(full, -1) {
				if luhnValid(c) {
					t.Fatalf("card number %q shown in %q", c, full)
				}
			}
			if u := userinfoPassword.FindString(full); u != "" {
				t.Fatalf("userinfo %q shown in %q", u, full)
			}
		}
	})
}
