package pushward

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

type callbackVector struct {
	IntegrationKey   string `json:"integration_key"`
	SecretHex        string `json:"secret_hex"`
	Whsec            string `json:"whsec"`
	WebhookID        string `json:"webhook_id"`
	WebhookTimestamp string `json:"webhook_timestamp"`
	Body             string `json:"body"`
	WebhookSignature string `json:"webhook_signature"`
}

// loadCallbackVectors reads the shared vector file the server signs against;
// testdata/callback-vectors-v1.json is a byte-identical copy.
func loadCallbackVectors(t *testing.T) []callbackVector {
	t.Helper()
	data, err := os.ReadFile("testdata/callback-vectors-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Format string           `json:"format"`
		Cases  []callbackVector `json:"cases"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.Format != "pushward-callback-v1" || len(f.Cases) == 0 {
		t.Fatalf("unexpected vector file: format %q, %d cases", f.Format, len(f.Cases))
	}
	return f.Cases
}

func vectorRequest(v callbackVector) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/hook", strings.NewReader(v.Body))
	r.Header.Set("webhook-id", v.WebhookID)
	r.Header.Set("webhook-timestamp", v.WebhookTimestamp)
	r.Header.Set("webhook-signature", v.WebhookSignature)
	return r
}

func vectorTime(t *testing.T, v callbackVector) time.Time {
	t.Helper()
	sec, err := strconv.ParseInt(v.WebhookTimestamp, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return time.Unix(sec, 0)
}

func TestCallbackVectors(t *testing.T) {
	for _, v := range loadCallbackVectors(t) {
		t.Run(v.WebhookID, func(t *testing.T) {
			if got := hex.EncodeToString(callbackKey(v.IntegrationKey)); got != v.SecretHex {
				t.Errorf("secret = %s, want %s", got, v.SecretHex)
			}
			if got := CallbackSecret(v.IntegrationKey); got != v.Whsec {
				t.Errorf("CallbackSecret = %s, want %s", got, v.Whsec)
			}
			// Inside the tolerance on either side.
			for _, skew := range []time.Duration{0, CallbackTolerance, -CallbackTolerance} {
				ev, err := verifyCallback(vectorRequest(v), v.IntegrationKey, vectorTime(t, v).Add(skew))
				if err != nil {
					t.Fatalf("skew %s: %v", skew, err)
				}
				var want struct {
					Type string `json:"type"`
					Data struct {
						NotificationID int64  `json:"notification_id"`
						Status         string `json:"status"`
					} `json:"data"`
				}
				if err := json.Unmarshal([]byte(v.Body), &want); err != nil {
					t.Fatal(err)
				}
				if ev.Type != want.Type || ev.Data.NotificationID != want.Data.NotificationID || ev.Data.Status != want.Data.Status || ev.Timestamp.IsZero() {
					t.Errorf("event = %+v, want %+v", ev, want)
				}
			}
		})
	}
}

func TestVerifyCallbackRefuses(t *testing.T) {
	vectors := loadCallbackVectors(t)
	v, other := vectors[0], vectors[1]
	now := vectorTime(t, v)
	for name, tc := range map[string]struct {
		mutate func(r *http.Request)
		key    string
		now    time.Time
		want   error
	}{
		"other key": {key: other.IntegrationKey, want: ErrCallbackSignature},
		"tampered body": {mutate: func(r *http.Request) {
			r.Body = httptest.NewRequest("POST", "/", strings.NewReader(strings.Replace(v.Body, "4242", "4243", 1))).Body
		}, want: ErrCallbackSignature},
		"other id":        {mutate: func(r *http.Request) { r.Header.Set("webhook-id", "pwr_1") }, want: ErrCallbackSignature},
		"other signature": {mutate: func(r *http.Request) { r.Header.Set("webhook-signature", other.WebhookSignature) }, want: ErrCallbackSignature},
		"v2 entry only": {mutate: func(r *http.Request) {
			r.Header.Set("webhook-signature", strings.Replace(v.WebhookSignature, "v1,", "v2,", 1))
		}, want: ErrCallbackSignature},
		"timestamp shifted": {mutate: func(r *http.Request) { r.Header.Set("webhook-timestamp", "1791000001") }, now: now, want: ErrCallbackSignature},
		"too old":           {now: now.Add(CallbackTolerance + time.Second)},
		"from the future":   {now: now.Add(-CallbackTolerance - time.Second)},
		"no signature":      {mutate: func(r *http.Request) { r.Header.Del("webhook-signature") }},
		"no id":             {mutate: func(r *http.Request) { r.Header.Del("webhook-id") }},
		"timestamp not int": {mutate: func(r *http.Request) { r.Header.Set("webhook-timestamp", "2026-10-03T07:20:00Z") }},
		"body over limit": {mutate: func(r *http.Request) {
			r.Body = httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat(" ", maxCallbackBody+1))).Body
		}},
	} {
		t.Run(name, func(t *testing.T) {
			r := vectorRequest(v)
			if tc.mutate != nil {
				tc.mutate(r)
			}
			key := v.IntegrationKey
			if tc.key != "" {
				key = tc.key
			}
			at := now
			if !tc.now.IsZero() {
				at = tc.now
			}
			ev, err := verifyCallback(r, key, at)
			if err == nil {
				t.Fatalf("verified %+v, want an error", ev)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// Standard Webhooks senders list several signatures while a secret rotates;
// one match is enough.
func TestVerifyCallbackAnyOfSeveralSignatures(t *testing.T) {
	vectors := loadCallbackVectors(t)
	v := vectors[0]
	r := vectorRequest(v)
	r.Header.Set("webhook-signature", "v1,bm90LWl0 v1a,"+strings.TrimPrefix(v.WebhookSignature, "v1,")+" "+v.WebhookSignature)
	if _, err := verifyCallback(r, v.IntegrationKey, vectorTime(t, v)); err != nil {
		t.Fatal(err)
	}
}

// VerifyCallback checks against the real clock: a callback signed now
// passes, and the 2026 vectors are refused as stale once they are.
func TestVerifyCallbackUsesTheClock(t *testing.T) {
	const key = "hlk_0123456789abcdef0123456789abcdef"
	body := `{"type":"notification.expired","timestamp":"2026-10-03T08:20:00Z","data":{"notification_id":9,"status":"expired"}}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, callbackKey(key))
	mac.Write([]byte("pwr_9." + ts + "." + body))
	r := httptest.NewRequest(http.MethodPost, "/hook", strings.NewReader(body))
	r.Header.Set("webhook-id", "pwr_9")
	r.Header.Set("webhook-timestamp", ts)
	r.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	ev, err := VerifyCallback(r, key)
	if err != nil || ev.Type != CallbackEventExpired || ev.Data.Status != ReceiptStatusExpired {
		t.Fatalf("fresh callback: %+v, %v", ev, err)
	}

	v := loadCallbackVectors(t)[0]
	if time.Since(vectorTime(t, v)).Abs() > CallbackTolerance {
		if _, err := VerifyCallback(vectorRequest(v), v.IntegrationKey); err == nil {
			t.Error("a stale vector verified against the real clock")
		}
	}
}
