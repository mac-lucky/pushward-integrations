package universal

import (
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
		{"url", "https://exa mple.com/", TypeString, "[url]"},

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
			for _, u := range urlIn.FindAllString(out, -1) {
				if strings.ContainsAny(u, "?#") {
					t.Fatalf("URL %q inside %q shown with its query or fragment: %q", u, v, out)
				}
			}
		}
	})
}
