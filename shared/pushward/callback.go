package pushward

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Callback event types (CallbackEvent.Type). Each receipt fires at most one.
const (
	CallbackEventAcknowledged = "notification.acknowledged"
	CallbackEventExpired      = "notification.expired"
)

// CallbackEvent is the body PushWard POSTs to a notification's CallbackURL
// once it is acknowledged or expires. Data is the receipt at that moment.
// Delivery is at least once: a retry repeats the same webhook-id
// (pwr_<notification id>), so act on an event idempotently.
type CallbackEvent struct {
	Type      string              `json:"type"`
	Timestamp time.Time           `json:"timestamp"`
	Data      NotificationReceipt `json:"data"`
}

// CallbackTolerance is how far a callback's webhook-timestamp may be from
// the receiver's clock, either way, before VerifyCallback refuses it as a
// possible replay.
const CallbackTolerance = 5 * time.Minute

// maxCallbackBody bounds what VerifyCallback reads. A receipt with ten tags
// is under 2 KiB.
const maxCallbackBody = 64 << 10

// ErrCallbackSignature is returned by VerifyCallback when no signature on
// the request matches the integration key.
var ErrCallbackSignature = errors.New("pushward: callback signature does not match")

// callbackKey is the HMAC key callbacks are signed with. It is derived from
// the key's SHA-256, the only form of the key the server keeps, so there is
// no extra secret to store and rolling the key changes it.
func callbackKey(integrationKey string) []byte {
	sum := sha256.Sum256([]byte(integrationKey))
	mac := hmac.New(sha256.New, sum[:])
	mac.Write([]byte("pushward/callback/v1"))
	return mac.Sum(nil)
}

// CallbackSecret returns the whsec_ secret that signs the callbacks of
// notifications sent with integrationKey, the form Standard Webhooks
// libraries take.
func CallbackSecret(integrationKey string) string {
	return "whsec_" + base64.StdEncoding.EncodeToString(callbackKey(integrationKey))
}

// VerifyCallback reads the body of a callback request and checks it was
// signed for integrationKey, the hlk_ key that sent the notification (an
// empty or other value is refused before the body is read): the
// Standard Webhooks headers webhook-id, webhook-timestamp (within
// CallbackTolerance of now) and webhook-signature ("v1,<base64>" entries,
// any one matching). It returns the decoded event, or ErrCallbackSignature
// for a request this key did not sign. The body is consumed either way.
func VerifyCallback(r *http.Request, integrationKey string) (*CallbackEvent, error) {
	return verifyCallback(r, integrationKey, time.Now())
}

func verifyCallback(r *http.Request, integrationKey string, now time.Time) (*CallbackEvent, error) {
	// Only integration keys can ask for callbacks. Refusing anything else
	// first keeps a missing config value from becoming a known HMAC key.
	if !strings.HasPrefix(integrationKey, "hlk_") || len(integrationKey) == len("hlk_") {
		return nil, errors.New("pushward: VerifyCallback needs the hlk_ integration key that sent the notification")
	}
	id := r.Header.Get("webhook-id")
	ts := r.Header.Get("webhook-timestamp")
	sigs := r.Header.Get("webhook-signature")
	if id == "" || ts == "" || sigs == "" {
		return nil, errors.New("pushward: callback lacks webhook-id, webhook-timestamp or webhook-signature")
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("pushward: callback webhook-timestamp %q is not unix seconds", ts)
	}
	if skew := now.Sub(time.Unix(sec, 0)); skew > CallbackTolerance || skew < -CallbackTolerance {
		return nil, fmt.Errorf("pushward: callback timestamp is %s from now, over the %s tolerance", skew.Abs().Round(time.Second), CallbackTolerance)
	}
	if r.Body == nil {
		return nil, errors.New("pushward: callback has no body")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCallbackBody+1))
	if err != nil {
		return nil, fmt.Errorf("pushward: reading callback body: %w", err)
	}
	if len(body) > maxCallbackBody {
		return nil, fmt.Errorf("pushward: callback body is over %d bytes", maxCallbackBody)
	}
	mac := hmac.New(sha256.New, callbackKey(integrationKey))
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	if !signatureMatches(sigs, mac.Sum(nil)) {
		return nil, ErrCallbackSignature
	}
	var ev CallbackEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, fmt.Errorf("pushward: decoding callback: %w", err)
	}
	return &ev, nil
}

// signatureMatches checks the space-separated "v1,<base64>" entries of a
// webhook-signature header against want. Entries of another version are
// skipped, as Standard Webhooks asks.
func signatureMatches(header string, want []byte) bool {
	for _, entry := range strings.Fields(header) {
		version, sig, ok := strings.Cut(entry, ",")
		if !ok || version != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(sig)
		if err == nil && hmac.Equal(got, want) {
			return true
		}
	}
	return false
}
