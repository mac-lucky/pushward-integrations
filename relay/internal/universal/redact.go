package universal

import (
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// ValueClass is what a field holds, judged from one sample. It decides which
// roles the field may play and how its sample is shown to the user.
type ValueClass string

const (
	ClassText   ValueClass = "text"
	ClassEnum   ValueClass = "enum"
	ClassURL    ValueClass = "url"
	ClassID     ValueClass = "id"
	ClassNumber ValueClass = "number"
	ClassBool   ValueClass = "bool"
	ClassTime   ValueClass = "time"
	ClassEmail  ValueClass = "email"
	ClassSecret ValueClass = "secret"
	ClassEmpty  ValueClass = "empty"
)

// Placeholders Display puts where a value may not be shown.
const (
	redacted    = "[redacted]"
	maskedEmail = "[email]"
)

// credentials are the formats a credential is known by: PushWard's own keys,
// GitHub, GitLab, Slack, OpenAI-style, Stripe keys and webhook secrets,
// Mailgun, Twilio API keys, AWS access key ids and JWTs.
const credentials = `hl[ka]_[A-Za-z0-9_-]{4,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|` +
	`glpat-[A-Za-z0-9_-]{20,}|xox[a-z]-[A-Za-z0-9-]{10,}|sk-[A-Za-z0-9_-]{20,}|` +
	`[rs]k_(?:live|test)_[A-Za-z0-9]{10,}|whsec_[A-Za-z0-9+/=]{20,}|key-[0-9a-z]{32}|SK[0-9a-fA-F]{32}|` +
	`(?:AKIA|ASIA)[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{10,}={0,2}(?:\.[A-Za-z0-9_=-]*){0,2}`

var (
	// credentialWord matches a key that starts with a credential;
	// credentialIn finds them inside text ("token=ghp_...").
	credentialWord = regexp.MustCompile(`^(?:` + credentials + `)`)
	credentialIn   = regexp.MustCompile(`\b(?:` + credentials + `)`)
	emailValue     = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)
	emailIn        = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	privateKeyPEM  = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	smallInt       = regexp.MustCompile(`^[0-9]{1,2}$`)

	// What Display masks inside text. The ones with a group mask only the
	// group: the credential after "Bearer" or "password=".
	urlIn      = regexp.MustCompile(`(?i)https?://\S+`)
	authScheme = regexp.MustCompile(`(?i)\b(?:basic|bearer|token)\s+(\S{8,})`)
	keyValue   = regexp.MustCompile(`(?i)\b[\w.-]*?(?:password|passwd|pwd|secret|token|api[_-]?key)["']?\s*[=:]\s*("[^"]*"|'[^']*'|[^\s"',;&]+)`)
	hexIn      = regexp.MustCompile(`[0-9A-Fa-f]{32,}`)
	cardIn     = regexp.MustCompile(`\b[0-9](?:[ -]?[0-9]){12,18}\b`)
	ibanIn     = regexp.MustCompile(`\b[A-Z]{2}[0-9]{2}(?: ?[A-Z0-9]){11,30}\b`)
	phoneIn    = regexp.MustCompile(`\+[0-9](?:[ ().-]{0,2}[0-9]){7,}`)
)

// Words that name a credential when they end a key. Keys written as one
// lower case word ("accesstoken", "xapikey") are caught by the suffix list.
var (
	secretWords = map[string]bool{
		"token": true, "secret": true, "password": true, "passwd": true, "pwd": true,
		"pass": true, "passcode": true, "pin": true, "otp": true,
		"apikey": true, "auth": true, "authorization": true, "signature": true,
		"cookie": true, "session": true, "private": true, "credential": true,
		"privatekey": true, "secretkey": true, "accesskey": true, "passphrase": true,
	}
	secretSuffixes = []string{"token", "secret", "password", "passwd", "apikey", "signature", "cookie", "credential"}
	// "api_key", "privateKey", "secret-key", "AccessKey", "hmac_key".
	keyQualifiers = map[string]bool{
		"api": true, "private": true, "secret": true, "access": true, "signing": true,
		"auth": true, "encryption": true, "webhook": true, "license": true,
		"client": true, "master": true, "hmac": true, "ssh": true,
	}
	// Leaf keys that take their meaning from the parent: token.value,
	// secrets[].value, apiKey.key.
	genericKeys = map[string]bool{"value": true, "values": true, "val": true, "key": true, "data": true}
)

// ClassOf classifies one field. A key that names a credential makes the field
// a secret whatever it holds, and so does a value that holds a private key or
// is, or carries in at most two words, a JWT, a known API key or a generated
// token ("Bearer eyJ..."). A generated token under a key that says it is an
// id ("charge_id": "ch_...") is an id instead: services use such ids far more
// often than they leak credentials under id keys. Integers of one or two
// digits are numbers even as strings ("3" for a severity level).
func ClassOf(path string, f Field) ValueClass {
	if secretPath(path) {
		return ClassSecret
	}
	switch f.Type {
	case TypeNull, TypeEmpty:
		return ClassEmpty
	case TypeBool:
		return ClassBool
	case TypeNumber:
		return ClassNumber
	}
	v := strings.TrimSpace(f.Value)
	if v == "" {
		return ClassEmpty
	}
	if strings.Contains(v, "PRIVATE KEY-----") && privateKeyPEM.MatchString(v) {
		return ClassSecret
	}
	// A pre-signed or tokened link is still a link; Display drops its query
	// and masks what is secret in its path.
	if lv := strings.ToLower(v); strings.HasPrefix(lv, "http://") || strings.HasPrefix(lv, "https://") {
		return ClassURL
	}
	if countWords(v) <= 2 {
		if credentialIn.MatchString(v) {
			return ClassSecret
		}
		for _, w := range strings.Fields(v) {
			if randomToken(w) && !endsInID(lastKey(path)) {
				return ClassSecret
			}
		}
	}
	switch {
	case emailValue.MatchString(v):
		return ClassEmail
	case timeValue.MatchString(v):
		return ClassTime
	case smallInt.MatchString(v):
		return ClassNumber
	case uuidValue.MatchString(v), hexValue.MatchString(v) && strings.ContainsAny(v, "0123456789"),
		intValue.MatchString(v), randomToken(v):
		return ClassID
	case enumValue.MatchString(v):
		return ClassEnum
	}
	return ClassText
}

// secretPath reports whether a field's key, or the parent of a generic leaf
// key, names a credential. A secret that only its value gives away is not
// one: such a field may still be a correlation id, which is hashed and never
// shown.
func secretPath(path string) bool {
	segs := segments(path)
	if len(segs) == 0 {
		return false
	}
	leaf := segs[len(segs)-1]
	if secretKey(leaf) {
		return true
	}
	return len(segs) > 1 && genericKeys[strings.ToLower(leaf)] && secretKey(segs[len(segs)-2])
}

// endsInID reports whether a key's last word says it holds an identifier:
// id, uid, uuid, ref, a bare key, or a word ending in id ("userid").
func endsInID(key string) bool {
	toks := tokens(key)
	if len(toks) == 0 {
		return false
	}
	switch last := toks[len(toks)-1]; last {
	case "ids", "fingerprint", "ref", "reference", "key":
		return true
	default:
		return strings.HasSuffix(last, "id")
	}
}

// digestName reports whether a key names an identifier or a digest, whose
// long hex Display may show.
func digestName(key string) bool {
	if endsInID(key) {
		return true
	}
	for _, t := range tokens(key) {
		switch strings.TrimRight(t, "0123456789") {
		case "id", "sha", "hash", "digest", "commit", "revision", "checksum":
			return true
		}
	}
	return false
}

// secretKey reports whether a key names a credential. The credential word
// has to be the key's last one ("access_token", "X-Api-Key",
// "X-Hub-Signature-256"), so a key that only describes one ("token_id",
// "session_name", "authorizationStatus") is judged by its value instead.
func secretKey(key string) bool {
	toks := tokens(key)
	for len(toks) > 0 && strings.TrimRight(toks[len(toks)-1], "0123456789") == "" {
		toks = toks[:len(toks)-1]
	}
	if len(toks) == 0 {
		return false
	}
	last := strings.TrimRight(toks[len(toks)-1], "0123456789")
	one := strings.TrimSuffix(last, "s")
	if one == "key" {
		return len(toks) > 1 && keyQualifiers[toks[len(toks)-2]]
	}
	if secretWords[last] || secretWords[one] {
		return true
	}
	for _, w := range secretSuffixes {
		if strings.HasSuffix(one, w) {
			return true
		}
	}
	return false
}

// randomToken reports whether s reads as generated rather than written: 20 or
// more characters from the base64 alphabet, at least two of them digits,
// letters that are mostly consonants, and at least 3.5 bits of entropy per
// character. Entropy alone is not enough: most keys and enum values of that
// length ("is_auto_renew_enabled_on_trial_end") clear 3.5 bits too, and
// the vowel test is what tells them from a token. Uuids and plain hex are
// left to the id rules.
func randomToken(s string) bool {
	if len(s) < 20 || uuidValue.MatchString(s) || hexValue.MatchString(s) {
		return false
	}
	var counts [128]uint16
	digits, letters, vowels := 0, 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case '0' <= c && c <= '9':
			digits++
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
			letters++
			switch c | 0x20 {
			case 'a', 'e', 'i', 'o', 'u':
				vowels++
			}
		case tokenByte(c):
		default:
			return false
		}
		counts[c]++
	}
	if digits < 2 || letters == 0 || 10*vowels >= 3*letters {
		return false
	}
	n := float64(len(s))
	bits := 0.0
	for _, c := range counts {
		if c > 0 {
			p := float64(c) / n
			bits -= p * math.Log2(p)
		}
	}
	return bits >= 3.5
}

// anyRun reports whether fn holds for a maximal run of the bytes in accepts.
func anyRun(s string, in func(byte) bool, fn func(string) bool) bool {
	start := -1
	for i := 0; i <= len(s); i++ {
		if i < len(s) && in(s[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 && fn(s[start:i]) {
			return true
		}
		start = -1
	}
	return false
}

// Display renders a field's sample for the review notification and the
// editor. Secrets and emails are replaced whole. A URL keeps its scheme, host
// and path, with userinfo, query, fragment and path parameters dropped and
// the secret parts of its host and path shown as "...". Everything else is
// cut to maxRunes with what looks secret inside it masked: credentials,
// generated tokens, long hex (unless the key names an id or a digest), emails,
// card numbers, IBANs and phone numbers; URLs inside it are shown as above.
//
// Values arrive already cut to MaxValueRunes by Flatten, so a credential cut
// in half by that cap can slip past the patterns.
func Display(path string, f Field, maxRunes int) string {
	v := strings.TrimSpace(f.Value)
	switch ClassOf(path, f) {
	case ClassSecret:
		return redacted
	case ClassEmail:
		return maskedEmail
	case ClassEmpty:
		return ""
	case ClassBool:
		return v
	case ClassURL:
		return text.Truncate(displayURL(v), maxRunes)
	}
	return text.Truncate(mask(v, digestName(lastKey(path))), maxRunes)
}

func displayURL(v string) string {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return "[url]"
	}
	labels := strings.Split(u.Host, ".")
	for i, l := range labels {
		if secretPart(l) {
			labels[i] = "..."
		}
	}
	segs := strings.Split(u.EscapedPath(), "/")
	for i, s := range segs {
		// Path parameters (";jsessionid=...") go the way of the query.
		if j := strings.IndexByte(s, ';'); j >= 0 {
			s = s[:j]
		}
		raw, err := url.PathUnescape(s)
		// Long hex and uuids in a path are capability ids: an hc-ping check,
		// a Home Assistant webhook.
		if err != nil || secretPart(raw) || len(raw) >= 16 && (uuidValue.MatchString(raw) || hexValue.MatchString(raw)) {
			s = "..."
		}
		segs[i] = s
	}
	return u.Scheme + "://" + strings.Join(labels, ".") + strings.Join(segs, "/")
}

// secretPart reports whether a host label or path segment holds a
// credential, an email address, or a generated token in any of its runs
// ("bot123456789:AAH...").
func secretPart(s string) bool {
	return credentialIn.MatchString(s) || emailIn.MatchString(s) || anyRun(s, tokenByte, randomToken)
}

// mask masks what looks secret inside free text, keeping everything around
// it. URLs are shown as displayURL shows them. Joining the pieces back can
// put a credential's start right after a placeholder, so a last pass masks
// anything that still reads as one.
func mask(v string, keepHex bool) string {
	var b strings.Builder
	last := 0
	for _, m := range urlIn.FindAllStringIndex(v, -1) {
		b.WriteString(maskText(v[last:m[0]], keepHex))
		b.WriteString(displayURL(v[m[0]:m[1]]))
		last = m[1]
	}
	b.WriteString(maskText(v[last:], keepHex))
	out := b.String()
	for range 8 {
		m := credentialIn.ReplaceAllLiteralString(out, redacted)
		if m == out {
			break
		}
		out = m
	}
	return out
}

// maskText masks text without URLs. A placeholder can put a word boundary
// where there was none ("AKIA...eyJ..."), so it goes again until nothing is
// left to mask.
func maskText(v string, keepHex bool) string {
	for range 8 {
		m := maskOnce(v, keepHex)
		if m == v {
			break
		}
		v = m
	}
	return v
}

// span is a stretch of text to mask; email says it holds only an address.
type span struct {
	start, end int
	email      bool
}

func maskOnce(v string, keepHex bool) string {
	var spans []span
	add := func(re *regexp.Regexp, email bool, valid func(string) bool) {
		for _, m := range re.FindAllStringSubmatchIndex(v, -1) {
			if len(m) >= 4 && m[2] >= 0 {
				m = m[2:4]
			}
			if valid == nil || valid(v[m[0]:m[1]]) {
				spans = append(spans, span{m[0], m[1], email})
			}
		}
	}
	add(credentialIn, false, nil)
	add(emailIn, true, nil)
	add(authScheme, false, nil)
	add(keyValue, false, nil)
	add(cardIn, false, luhn)
	add(ibanIn, false, ibanValid)
	add(phoneIn, false, nil)
	if !keepHex {
		add(hexIn, false, nil)
	}
	for i := 0; i < len(v); {
		if !tokenByte(v[i]) {
			i++
			continue
		}
		j := i
		for j < len(v) && tokenByte(v[j]) {
			j++
		}
		if randomToken(v[i:j]) {
			spans = append(spans, span{i, j, false})
		}
		i = j
	}
	if len(spans) == 0 {
		return v
	}
	slices.SortFunc(spans, func(a, b span) int { return a.start - b.start })
	var b strings.Builder
	b.Grow(len(v))
	last := 0
	for i := 0; i < len(spans); {
		cur := spans[i]
		for i++; i < len(spans) && spans[i].start < cur.end; i++ {
			cur.end = max(cur.end, spans[i].end)
			cur.email = cur.email && spans[i].email
		}
		b.WriteString(v[last:cur.start])
		if cur.email {
			b.WriteString(maskedEmail)
		} else {
			b.WriteString(redacted)
		}
		last = cur.end
	}
	b.WriteString(v[last:])
	return b.String()
}

// luhn reports whether 13 to 19 digits, spaces and dashes aside, pass the
// card number checksum.
func luhn(s string) bool {
	sum, n, double := 0, 0, false
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c < '0' || c > '9' {
			continue
		}
		d := int(c - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum, n, double = sum+d, n+1, !double
	}
	return n >= 13 && n <= 19 && sum%10 == 0
}

// ibanValid checks an IBAN's mod-97 checksum.
func ibanValid(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	r := 0
	for _, c := range s[4:] + s[:4] {
		switch {
		case '0' <= c && c <= '9':
			r = (r*10 + int(c-'0')) % 97
		case 'A' <= c && c <= 'Z':
			r = (r*100 + int(c-'A'+10)) % 97
		default:
			return false
		}
	}
	return r == 1
}
