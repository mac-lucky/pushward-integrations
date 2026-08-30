package pushward

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Tap-action wire bounds, mirroring pushward-server's model.TapAction. A
// bridge that validates against these at config load turns a typo'd dashboard
// URL into a startup error instead of a 422 on the first push - which for the
// widget templates that publish eagerly is the difference between a message and
// a crash loop.
const (
	MaxTapActionURLRunes     = 2048
	MaxTapActionBodyRunes    = 1024
	MaxTapActionTitleRunes   = 64
	MaxTapActionIconRunes    = 64
	MaxTapActionHeadersBytes = 1024 // keys and values together
)

// TapActionMethods is the server's allow list of HTTP methods on a silent
// webhook action.
var TapActionMethods = []string{
	http.MethodGet,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
	http.MethodHead,
}

var allowedTapActionMethods = func() map[string]struct{} {
	m := make(map[string]struct{}, len(TapActionMethods))
	for _, v := range TapActionMethods {
		m[v] = struct{}{}
	}
	return m
}()

// blockedTapActionSchemes mirrors the server's reject list. iOS refuses to open
// these anyway; rejecting them here keeps them out of config files and logs as
// clickable links.
var blockedTapActionSchemes = map[string]struct{}{
	"javascript": {},
	"data":       {},
	"file":       {},
	"vbscript":   {},
}

// Validate enforces the TapAction invariants the server enforces, under the
// same field name it would report. A nil action is valid: the slot is unset.
//
// method/headers/body are http(s)-only. Pairing them with a custom scheme is
// rejected rather than silently dropped, because a custom scheme hands the URL
// to another app and there is nothing left to attach them to.
func (a *TapAction) Validate(field string) error {
	if a == nil {
		return nil
	}
	if a.URL == "" {
		return fmt.Errorf("%s.url is required", field)
	}
	if utf8.RuneCountInString(a.URL) > MaxTapActionURLRunes {
		return fmt.Errorf("%s.url must not exceed %d characters", field, MaxTapActionURLRunes)
	}
	// url.Parse lowercases the scheme per RFC 3986 section 3.1.
	u, err := url.Parse(a.URL)
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("%s.url must be a valid URL with a scheme", field)
	}
	if _, blocked := blockedTapActionSchemes[strings.ToLower(u.Scheme)]; blocked {
		return fmt.Errorf("%s.url uses blocked scheme %q", field, u.Scheme)
	}
	isHTTP := u.Scheme == "http" || u.Scheme == "https"
	if isHTTP && u.Host == "" {
		return fmt.Errorf("%s.url must include a host for http/https URLs", field)
	}
	if (a.Method != "" || len(a.Headers) > 0 || a.Body != "") && !isHTTP {
		return fmt.Errorf("%s: method/headers/body are only valid for http(s) URLs", field)
	}
	if a.Method != "" {
		if _, ok := allowedTapActionMethods[strings.ToUpper(a.Method)]; !ok {
			return fmt.Errorf("%s.method must be one of %s", field, strings.Join(TapActionMethods, ", "))
		}
	}
	if err := validateTapActionHeaders(a.Headers, field); err != nil {
		return err
	}
	if utf8.RuneCountInString(a.Body) > MaxTapActionBodyRunes {
		return fmt.Errorf("%s.body must not exceed %d characters", field, MaxTapActionBodyRunes)
	}
	if utf8.RuneCountInString(a.Title) > MaxTapActionTitleRunes {
		return fmt.Errorf("%s.title must not exceed %d characters", field, MaxTapActionTitleRunes)
	}
	if utf8.RuneCountInString(a.Icon) > MaxTapActionIconRunes {
		return fmt.Errorf("%s.icon must not exceed %d characters", field, MaxTapActionIconRunes)
	}
	return nil
}

func validateTapActionHeaders(headers map[string]string, field string) error {
	if len(headers) == 0 {
		return nil
	}
	var totalBytes int
	for k, v := range headers {
		if !validHeaderFieldName(k) {
			return fmt.Errorf("%s.headers key %q is not a valid HTTP token (RFC 7230)", field, k)
		}
		// RFC 9110 section 6.5 forbids CR, LF and NUL in field values.
		if strings.ContainsAny(v, "\r\n\x00") {
			return fmt.Errorf("%s.headers value for %q contains a forbidden control character (CR/LF/NUL)", field, k)
		}
		totalBytes += len(k) + len(v)
	}
	if totalBytes > MaxTapActionHeadersBytes {
		return fmt.Errorf("%s.headers total size must not exceed %d bytes", field, MaxTapActionHeadersBytes)
	}
	return nil
}

// validHeaderFieldName reports whether s is an RFC 7230 token. Spelled out
// rather than pulled from golang.org/x/net/http/httpguts: shared is a
// dependency of all eight modules and carries one direct requirement today.
func validHeaderFieldName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// ValidateTapActionSlots validates the three tap-action slots a Content or
// WidgetContent can carry, in a fixed order so two bad slots always report the
// same one. The field names are owned here, matching the server's.
func ValidateTapActionSlots(tap, primary, secondary *TapAction) error {
	if err := tap.Validate("tap_action"); err != nil {
		return err
	}
	if err := primary.Validate("url_action"); err != nil {
		return err
	}
	return secondary.Validate("secondary_url_action")
}
