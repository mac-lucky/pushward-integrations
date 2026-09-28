package universal

import (
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"

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

// Known credential formats. The prefixed ones are distinctive enough to be
// found inside a word ("x_sk_live_..."); the bounded ones need a word boundary
// in front. Between them: PushWard's own keys, GitHub, GitLab, Slack,
// OpenAI-style, Stripe, Mailgun, Twilio, AWS, Google, SendGrid, npm, Linear,
// Shopify, New Relic and JWTs.
const (
	prefixedCredentials = `github_pat_[A-Za-z0-9_]{20,}|gl(?:pat|rt|dt|ptt|cbt|soat)-[\w-]{20,}|xox[a-z]-[A-Za-z0-9-]{10,}|` +
		`[rs]k_(?:live|test)_[A-Za-z0-9]{10,}|whsec_[A-Za-z0-9+/=]{20,}|lin_api_[A-Za-z0-9]{40}|shp(?:at|ss|ca|pa)_[0-9a-f]{32}`
	boundedCredentials = `hl[ka]_[A-Za-z0-9_-]{4,}|gh[pousr]_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9_-]{20,}|key-[0-9a-z]{32}|` +
		`SK[0-9a-fA-F]{32}|(?:AKIA|ASIA)[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|SG\.[\w-]{22}\.[\w-]{43}|npm_[A-Za-z0-9]{36}|` +
		`NRAK-[A-Z0-9]{27}|` +
		`eyJ[A-Za-z0-9_-]{10,}={0,2}(?:\.[A-Za-z0-9_=-]*){0,2}`
)

var (
	// credentialWord matches a key that starts with a credential;
	// credentialIn finds them inside text ("token=ghp_...").
	credentialWord = regexp.MustCompile(`^(?:` + boundedCredentials + `|` + prefixedCredentials + `)`)
	credentialIn   = regexp.MustCompile(`\b(?:` + boundedCredentials + `)|(?:` + prefixedCredentials + `)`)
	emailValue     = regexp.MustCompile(`^[\p{L}\p{N}._%+-]+@[\p{L}\p{N}.-]+\.\p{L}{2,}$`)
	emailIn        = regexp.MustCompile(`[\p{L}\p{N}._%+-]+@[\p{L}\p{N}.-]+\.\p{L}{2,}`)
	atEmailIn      = regexp.MustCompile(`(?i)[\p{L}\p{N}._%+-]+ ?[(\[]at[)\]] ?[\p{L}\p{N}.-]+\.\p{L}{2,}`)
	privateKeyPEM  = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	smallInt       = regexp.MustCompile(`^[0-9]{1,2}$`)
	versionWord    = regexp.MustCompile(`^v[0-9]+$`)

	// What Display masks inside text. The ones with a group mask only the
	// group: the credential after "Bearer" or "password=", the userinfo of
	// "postgres://user:pass@db".
	urlIn      = regexp.MustCompile("(?i)https?://[^\\s\"'<>\\\\{}|^`]+")
	authScheme = regexp.MustCompile(`(?i)\b(?:basic|bearer|token)\s+(\S{8,})`)
	keyValue   = regexp.MustCompile(`(?i)\b(?:[\w.-]*[_.-])?(?:password|passwd|pwd|pass|pin|otp|passphrase|secret|token|api[_-]?key|key|credentials?|private[_-]?key|sig)` +
		`\\?["']?\s*(?:=>|[=:])\s*(\\?"[^"]*"|'[^']*'|\\?["']?[^\s"'\\]+)`)
	// "--password hunter2" and "mysql -u root -phunter2".
	cliSecret  = regexp.MustCompile(`(?i)(?:--(?:password|passwd|pass|token|secret|api-?key)\s+|\b(?:mysql|mysqldump|mariadb)\b[^\n]*?\s-p)([^\s-]\S*)`)
	userinfoIn = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://([^/?#\s@]*)@`)
	hexIn      = regexp.MustCompile(`[0-9A-Fa-f]{32,}`)
	cardIn     = regexp.MustCompile(`\b[0-9](?:[ -]?[0-9]){12,18}\b`)
	ibanIn     = regexp.MustCompile(`(?i)\b[a-z]{2}[0-9]{2}(?: ?[a-z0-9]){11,30}\b`)
	phoneIn    = regexp.MustCompile(`\+[0-9](?:[ ().-]{0,2}[0-9]){7,}`)
	usPhoneIn  = regexp.MustCompile(`(?:\([0-9]{3}\) ?|\b[0-9]{3}[-.])[0-9]{3}[-.][0-9]{4}\b`)
	ssnIn      = regexp.MustCompile(`\b[0-9]{3}-[0-9]{2}-[0-9]{4}\b`)
)

// Words that name a credential when they end a key. Keys written as one
// lower case word ("accesstoken", "xapikey") are caught by the suffix list.
var (
	secretWords = map[string]bool{
		"token": true, "secret": true, "password": true, "passwd": true, "pwd": true,
		"pass": true, "passcode": true, "pin": true, "otp": true, "pat": true,
		"apikey": true, "auth": true, "authorization": true, "signature": true, "sig": true,
		"cookie": true, "session": true, "private": true, "credential": true, "cred": true, "creds": true,
		"privatekey": true, "privkey": true, "secretkey": true, "accesskey": true, "passphrase": true,
		"jwt": true, "bearer": true, "dsn": true, "mnemonic": true, "seed": true, "hmac": true,
		"sk": true,
		// Personal data a notification has no business showing.
		"ssn": true, "cvv": true, "cvc": true, "iban": true, "dob": true, "phone": true,
		"birthdate": true, "birthday": true,
	}
	secretSuffixes = []string{"token", "secret", "password", "passwd", "apikey", "signature", "cookie", "credential"}
	// Names of two or three words, checked before trailing words are dropped
	// ("connectionString" ends in "string").
	secretPhrases = map[string]bool{
		"database_url": true, "connection_string": true, "conn_str": true, "mongo_uri": true,
		"backup_code": true, "recovery_code": true, "access_code": true, "auth_code": true,
		"authorization_code": true, "mfa_code": true, "verification_code": true,
		"service_account": true, "pass_word": true,
		"tax_id": true, "card_number": true, "account_number": true, "routing_number": true,
		"phone_number": true, "date_of_birth": true,
	}
	// Trailing words that describe a secret's form or role, not what it is:
	// token_value, password_confirm, api_key_b64.
	secretTrailers = map[string]bool{
		"value": true, "values": true, "val": true, "string": true, "str": true,
		"b64": true, "base64": true, "pem": true, "encoded": true, "json": true,
		"hex": true, "raw": true, "plain": true, "header": true, "confirm": true,
		"confirmation": true, "repeat": true, "new": true, "old": true, "hash": true,
	}
	// "api_key", "privateKey", "secret-key", "AccessKey", "hmac_key".
	keyQualifiers = map[string]bool{
		"api": true, "private": true, "secret": true, "access": true, "signing": true,
		"auth": true, "encryption": true, "webhook": true, "license": true,
		"client": true, "master": true, "hmac": true, "ssh": true, "account": true, "sa": true,
	}
	// Leaf keys that take their meaning from the parent: token.value,
	// secrets[].value, apiKey.key.
	genericKeys = map[string]bool{"value": true, "values": true, "val": true, "key": true, "data": true}
	// Parents whose every child is secret: cookies.session_id,
	// credentials.username.
	secretParents = map[string]bool{"cookie": true, "secret": true, "credential": true}
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
	words := countWords(v)
	// A pre-signed or tokened link is still a link; Display drops its query
	// and masks what is secret in its path. A link with text after it is
	// text.
	if lv := strings.ToLower(v); words == 1 && (strings.HasPrefix(lv, "http://") || strings.HasPrefix(lv, "https://")) {
		return ClassURL
	}
	if words <= 2 {
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

// secretPath reports whether a field's key names a credential, or its
// parent does for a generic leaf key, or an ancestor is a cookie, secret or
// credential object. A secret that only its value gives away is not one:
// such a field may still be a correlation id, which is hashed and never
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
	for _, p := range segs[:len(segs)-1] {
		if secretParents[strings.TrimSuffix(lastToken(p), "s")] {
			return true
		}
	}
	if len(segs) > 1 && genericKeys[strings.ToLower(leaf)] {
		parent := segs[len(segs)-2]
		// api.key, hmac.key
		return secretKey(parent) || strings.EqualFold(leaf, "key") && keyQualifiers[lastToken(parent)]
	}
	return false
}

func lastToken(key string) string {
	toks := tokens(key)
	if len(toks) == 0 {
		return ""
	}
	return strings.TrimRight(toks[len(toks)-1], "0123456789")
}

// idWord reports whether one key word says it holds an identifier: uid,
// uuid, ref, a word ending in id ("userid").
func idWord(t string) bool {
	switch t {
	case "ids", "fingerprint", "ref", "reference":
		return true
	}
	return strings.HasSuffix(t, "id")
}

// endsInID reports whether a key's last word says it holds an identifier;
// a bare "key" counts ("pipelineKey").
func endsInID(key string) bool {
	toks := tokens(key)
	if len(toks) == 0 {
		return false
	}
	last := toks[len(toks)-1]
	return last == "key" || idWord(last)
}

// digestName reports whether a key names an identifier or a digest, whose
// long hex Display may show. A bare "key" does not: long hex under it is as
// likely a key as an id.
func digestName(key string) bool {
	toks := tokens(key)
	if len(toks) > 0 && idWord(toks[len(toks)-1]) {
		return true
	}
	for _, t := range toks {
		switch strings.TrimRight(t, "0123456789") {
		case "id", "sha", "hash", "digest", "commit", "revision", "checksum":
			return true
		}
	}
	return false
}

// secretKey reports whether a key names a credential. The credential word
// has to be the key's last one once version numbers and words about its form
// are dropped ("access_token", "X-Api-Key", "X-Hub-Signature-256",
// "password_confirm", "api_key_b64"), so a key that only describes one
// ("token_id", "session_name", "authorizationStatus") is judged by its value
// instead.
func secretKey(key string) bool {
	toks := tokens(key)
	for n := len(toks); n > 0; n = len(toks) {
		last := toks[n-1]
		one := strings.TrimSuffix(last, "s")
		if n > 1 && secretPhrases[toks[n-2]+"_"+one] || n > 2 && secretPhrases[toks[n-3]+"_"+toks[n-2]+"_"+one] {
			return true
		}
		if strings.TrimRight(last, "0123456789") != "" && !versionWord.MatchString(last) && !secretTrailers[last] {
			break
		}
		toks = toks[:n-1]
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
// length ("is_auto_renew_enabled_on_trial_end") clear 3.5 bits too, and the
// vowel test is what tells them from a token. Uuids and plain hex are left to
// the id rules.
func randomToken(s string) bool {
	return randomTokenVowels(s, 3, 10)
}

// randomTokenVowels is randomToken with the vowel share kept under num/den.
func randomTokenVowels(s string, num, den int) bool {
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
			if isVowel(c) {
				vowels++
			}
		case tokenByte(c):
		default:
			return false
		}
		counts[c]++
	}
	if digits < 2 || letters == 0 || den*vowels >= num*letters {
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

func isVowel(c byte) bool {
	switch c | 0x20 {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}

// mix describes the characters of a run of token bytes.
type mix struct {
	digit, upper, lower bool
	vowels, letters     int
}

func mixOf(s string) (mix, bool) {
	var m mix
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case '0' <= c && c <= '9':
			m.digit = true
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z':
			m.upper = m.upper || c <= 'Z'
			m.lower = m.lower || c >= 'a'
			m.letters++
			if isVowel(c) {
				m.vowels++
			}
		case !tokenByte(c):
			return m, false
		}
	}
	return m, true
}

// urlToken reports whether a run inside a URL host or path looks generated:
// 16 or more token bytes with upper and lower case letters and either a
// digit or few vowels. Slack and IFTTT hook keys are this; a readable path
// ("MyProjectReport") keeps its vowels.
func urlToken(s string) bool {
	if len(s) < 16 {
		return false
	}
	m, ok := mixOf(s)
	return ok && m.upper && m.lower && (m.digit || 10*m.vowels < 3*m.letters)
}

// keyToken is urlToken for a key piece, stricter because a key that reads as
// a token collapses to "*" with its siblings: digits, both cases, and fewer
// than a quarter vowels. "tbl..." and "rec..." ids pass; camelCase names with
// a number ("addressLine2Street") keep their vowels and do not.
func keyToken(s string) bool {
	if len(s) < 16 {
		return false
	}
	m, ok := mixOf(s)
	return ok && m.digit && m.upper && m.lower && 4*m.vowels < m.letters
}

// generated is what Display masks as a generated token, looser than
// randomToken because a false positive here costs a word of the sample, not a
// field: randomToken, a run of 20 or more that urlToken would take, or 24 or
// more lower-case letters and digits with at least four of each.
func generated(s string) bool {
	if randomToken(s) || len(s) >= 20 && urlToken(s) {
		return true
	}
	if len(s) < 24 || hexValue.MatchString(s) {
		return false
	}
	m, ok := mixOf(s)
	return ok && !m.upper && m.letters >= 4 && len(s)-m.letters >= 4 && strings.Trim(s, "abcdefghijklmnopqrstuvwxyz0123456789") == ""
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
// key=value secrets, generated tokens, long hex (unless the key names an id
// or a digest), userinfo in any URL, emails, card numbers, IBANs, phone and
// social security numbers; http(s) URLs inside it are shown as above.
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
		return text.Truncate(lastPass(displayURL(v)), maxRunes)
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
		if hiddenPart(l) || anyRun(l, pieceByte, generatedLabel) {
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
		if err != nil || hiddenPart(raw) {
			s = "..."
		}
		segs[i] = s
	}
	return u.Scheme + "://" + strings.Join(labels, ".") + strings.Join(segs, "/")
}

// generatedLabel reports a host label piece that no one typed: 16 or more
// characters mixing letters and digits (a tunnel's "o1e4h293f27xsvz8", a
// Pipedream "eo037f...").
func generatedLabel(p string) bool {
	return len(p) >= 16 && strings.ContainsAny(p, "0123456789") && strings.IndexFunc(p, unicode.IsLetter) >= 0
}

// hiddenPart reports whether a host label or path segment is shown as
// "...": it holds something secret, a long hex or uuid capability id (an
// hc-ping check, a Home Assistant webhook), or anything text masking would
// touch (/token=abc, /wh_<hex>, a card number).
func hiddenPart(s string) bool {
	return secretPart(s) || longID(s) || maskOnce(s, textDetectors, true, true) != s
}

// longID reports a uuid or a hex string of 16 or more characters.
func longID(s string) bool {
	return len(s) >= 16 && (uuidValue.MatchString(s) || hexValue.MatchString(s))
}

// secretPart reports whether a host label or path segment holds a
// credential, an email address, or a generated token in any of its runs
// ("bot123456789:AAH...").
func secretPart(s string) bool {
	return credentialIn.MatchString(s) || emailIn.MatchString(s) ||
		anyRun(s, tokenByte, func(r string) bool { return randomToken(r) || urlToken(r) })
}

// A detector finds stretches of text to mask: its first group when it has
// one, else the whole match. keep, when set, says how much of a candidate to
// mask, 0 for none: a checksum that fails, a value already masked.
type detector struct {
	re    *regexp.Regexp
	email bool
	keep  func(string) int
}

var (
	cardDetector  = detector{re: cardIn, keep: luhn}
	valueDetector = detector{re: keyValue, keep: unmasked}
	textDetectors = []detector{
		{re: credentialIn},
		{re: emailIn, email: true},
		{re: atEmailIn, email: true},
		{re: cliSecret},
		{re: authScheme},
		valueDetector,
		{re: userinfoIn},
		cardDetector,
		{re: ibanIn, keep: ibanPrefix},
		{re: phoneIn},
		{re: usPhoneIn},
		{re: ssnIn},
	}
	// lastDetectors run once more over the whole output, displayed URLs
	// included: joining the pieces back can put a boundary in front of
	// something that did not read as a secret inside its piece.
	lastDetectors = []detector{{re: credentialIn}, valueDetector, {re: userinfoIn}, cardDetector}
)

// unmasked skips a key=value value that is already a placeholder, perhaps
// with the punctuation that followed the quoted value ("[redacted]," in
// JSON), so a second pass does not eat that punctuation.
func unmasked(v string) int {
	if rest, ok := strings.CutPrefix(v, redacted); ok && strings.Trim(rest, ",;&)]}") == "" {
		return 0
	}
	return len(v)
}

// mask masks what looks secret inside free text, keeping everything around
// it. key=value secrets go first, since the value may itself be a URL; then
// http(s) URLs are shown as displayURL shows them and the text between them
// is masked; then a last pass over the whole.
func mask(v string, keepHex bool) string {
	v = fixpoint(v, func(s string) string { return maskOnce(s, []detector{valueDetector}, false, false) })
	var b strings.Builder
	last := 0
	for _, m := range urlIn.FindAllStringIndex(v, -1) {
		b.WriteString(fixpoint(v[last:m[0]], func(s string) string { return maskOnce(s, textDetectors, !keepHex, true) }))
		b.WriteString(displayURL(v[m[0]:m[1]]))
		last = m[1]
	}
	b.WriteString(fixpoint(v[last:], func(s string) string { return maskOnce(s, textDetectors, !keepHex, true) }))
	return lastPass(b.String())
}

// lastPass masks what reads as a credential, a key=value secret, userinfo or
// a card number in text already masked or displayed: a path segment can
// itself look like "x://user:pass@".
func lastPass(v string) string {
	return fixpoint(v, func(s string) string { return maskOnce(s, lastDetectors, false, false) })
}

// fixpoint applies f until nothing changes: a placeholder can put a word
// boundary where there was none ("AKIA...eyJ..."), so masking goes again.
func fixpoint(v string, f func(string) string) string {
	for range 8 {
		m := f(v)
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

// maskOnce masks what the detectors find, plus long hex and generated
// tokens when asked.
func maskOnce(v string, ds []detector, hex, runs bool) string {
	var spans []span
	add := func(d detector) {
		for _, m := range d.re.FindAllStringSubmatchIndex(v, -1) {
			if len(m) >= 4 && m[2] >= 0 {
				m = m[2:4]
			}
			n := m[1] - m[0]
			if d.keep != nil {
				n = d.keep(v[m[0]:m[1]])
			}
			if n > 0 {
				spans = append(spans, span{m[0], m[0] + n, d.email})
			}
		}
	}
	for _, d := range ds {
		add(d)
	}
	if hex {
		add(detector{re: hexIn})
	}
	for i := 0; runs && i < len(v); {
		if !tokenByte(v[i]) {
			i++
			continue
		}
		j := i
		for j < len(v) && tokenByte(v[j]) {
			j++
		}
		if generated(v[i:j]) {
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

// luhn keeps a card number whose 13 to 19 digits, spaces and dashes aside,
// pass the card number checksum.
func luhn(s string) int {
	if luhnValid(s) {
		return len(s)
	}
	return 0
}

func luhnValid(s string) bool {
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

// ibanPrefix keeps the longest IBAN at the start of a candidate: the
// pattern also takes a lower-case word that follows ("... 0130 00 soon"), so
// it is cut back at spaces until the checksum holds.
func ibanPrefix(s string) int {
	for end := len(s); end > 0; end = strings.LastIndexByte(s[:end], ' ') {
		if ibanValid(s[:end]) {
			return end
		}
	}
	return 0
}

// ibanValid checks an IBAN's mod-97 checksum, in either case and with or
// without spaces.
func ibanValid(s string) bool {
	s = strings.ToUpper(strings.ReplaceAll(s, " ", ""))
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
