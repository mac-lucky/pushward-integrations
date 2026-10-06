package pushward

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const receiptJSON = `{"notification_id":42,"status":"acknowledged","repeat_seconds":300,
	"expires_at":"2026-10-03T09:20:00Z","repeats_sent":2,"last_delivered_at":"2026-10-03T07:30:00Z",
	"acknowledged_at":"2026-10-03T07:31:00Z","acknowledged_by":"7f9c","acknowledged_by_device":"Kitchen iPad",
	"action_id":"pw_ack","tags":["nas-1"],
	"callback":{"status":"delivered","attempts":1,"delivered_at":"2026-10-03T07:31:01Z","last_status_code":204},
	"created_at":"2026-10-03T07:20:00Z"}`

func checkReceipt(t *testing.T, r *NotificationReceipt) {
	t.Helper()
	if r == nil {
		t.Fatal("no receipt")
	}
	if r.NotificationID != 42 || r.Status != ReceiptStatusAcknowledged || r.RepeatSeconds != 300 || r.RepeatsSent != 2 ||
		r.ActionID != AckActionID || r.AcknowledgedBy != "7f9c" || r.AcknowledgedByDevice != "Kitchen iPad" ||
		r.AcknowledgedAt == nil || r.LastDeliveredAt == nil || r.ExpiresAt.IsZero() || r.CreatedAt.IsZero() ||
		len(r.Tags) != 1 || r.Tags[0] != "nas-1" {
		t.Errorf("receipt = %+v", r)
	}
	if c := r.Callback; c == nil || c.Status != CallbackStatusDelivered || c.Attempts != 1 || c.DeliveredAt == nil || c.LastStatusCode != 204 {
		t.Errorf("callback = %+v", r.Callback)
	}
}

func TestSendNotificationResult_AcknowledgeAndReceipt(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":42,"pushed":true,"answerable":true,"delivery":"all","created_at":"2026-10-03T07:20:00Z","receipt":` + receiptJSON + `}`))
	}))
	defer srv.Close()

	sn, err := NewClient(srv.URL, "hlk_test").SendNotificationResult(context.Background(), SendNotificationRequest{
		Title:       "Backup failed on nas-1",
		Body:        "rsync exit 23",
		Level:       "time-sensitive",
		Acknowledge: &NotificationAcknowledge{},
		Tags:        []string{"nas-1"},
		CallbackURL: "https://hooks.example.com/pushward",
	})
	if err != nil {
		t.Fatalf("SendNotificationResult: %v", err)
	}
	checkReceipt(t, sn.Receipt)
	// An empty Acknowledge asks for every default: it must reach the server
	// as {}, not vanish.
	if ack, ok := got["acknowledge"].(map[string]any); !ok || len(ack) != 0 {
		t.Errorf("acknowledge = %#v, want {}", got["acknowledge"])
	}
	if tags, _ := got["tags"].([]any); len(tags) != 1 || tags[0] != "nas-1" || got["callback_url"] != "https://hooks.example.com/pushward" {
		t.Errorf("tags/callback_url = %v / %v", got["tags"], got["callback_url"])
	}
	if _, ok := got["encrypted"]; ok {
		t.Errorf("encrypted must be omitted when unset: %v", got)
	}
}

func TestSendNotification_NewFieldsOmittedWhenUnset(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	if err := NewClient(srv.URL, "hlk_test").SendNotification(context.Background(), SendNotificationRequest{Title: "t", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"encrypted", "acknowledge", "tags", "callback_url"} {
		if _, ok := got[key]; ok {
			t.Errorf("%s must be omitted when unset: %v", key, got)
		}
	}
}

// An encrypted send leaves the four sealed fields empty and passes the
// envelope through as given; the acknowledge settings ride along on a
// schedule through the embedded request.
func TestScheduleNotification_EncryptedAndAcknowledged(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7,"status":"scheduled","title":"Encrypted notification","body":"End-to-end encrypted.",
			"encrypted":"pw1.767c0806.AAAA","acknowledge":{"repeat_seconds":120},"tags":["standup"],
			"send_at":"2030-01-01T09:00:00Z","created_at":"2026-10-03T07:20:00Z"}`))
	}))
	defer srv.Close()

	sn, err := NewClient(srv.URL, "hlk_test").ScheduleNotification(context.Background(), ScheduleNotificationRequest{
		SendNotificationRequest: SendNotificationRequest{
			Encrypted:   "pw1.767c0806.AAAA",
			Acknowledge: &NotificationAcknowledge{RepeatSeconds: 120, ExpireSeconds: 600, ActionTitle: "Done"},
			Tags:        []string{"standup"},
		},
		SendAt: time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ScheduleNotification: %v", err)
	}
	if got["encrypted"] != "pw1.767c0806.AAAA" || got["title"] != "" || got["body"] != "" {
		t.Errorf("body = %v, want the envelope and empty title/body", got)
	}
	for _, key := range []string{"subtitle", "url"} {
		if _, ok := got[key]; ok {
			t.Errorf("%s must stay out of an encrypted send: %v", key, got)
		}
	}
	ack, _ := got["acknowledge"].(map[string]any)
	if ack["repeat_seconds"] != float64(120) || ack["expire_seconds"] != float64(600) || ack["action_title"] != "Done" {
		t.Errorf("acknowledge = %v", got["acknowledge"])
	}
	if sn.Encrypted != "pw1.767c0806.AAAA" || sn.Acknowledge == nil || sn.Acknowledge.RepeatSeconds != 120 || len(sn.Tags) != 1 {
		t.Errorf("echo = %+v", sn)
	}
}

func TestGetNotificationReceipt_WaitParamAndDecode(t *testing.T) {
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		_, _ = w.Write([]byte(receiptJSON))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "hlk_test")

	for _, tc := range []struct {
		wait time.Duration
		uri  string
	}{
		{0, "/notifications/receipts/42"},
		{-time.Second, "/notifications/receipts/42"},
		{1500 * time.Millisecond, "/notifications/receipts/42?wait=1"},
		{90 * time.Second, "/notifications/receipts/42?wait=25"},
	} {
		r, err := c.GetNotificationReceipt(context.Background(), 42, tc.wait)
		if err != nil {
			t.Fatalf("wait %s: %v", tc.wait, err)
		}
		if gotURI != tc.uri {
			t.Errorf("wait %s: request %s, want %s", tc.wait, gotURI, tc.uri)
		}
		checkReceipt(t, r)
	}
}

func TestCancelNotificationReceipts(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		if r.URL.Path == "/notifications/receipts/cancel" {
			_, _ = w.Write([]byte(`{"canceled":3}`))
			return
		}
		_, _ = w.Write([]byte(`{"notification_id":42,"status":"canceled","cancel_reason":"api","repeat_seconds":60,
			"expires_at":"2026-10-03T08:20:00Z","repeats_sent":4,"canceled_at":"2026-10-03T07:25:00Z","created_at":"2026-10-03T07:20:00Z"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "hlk_test")

	r, err := c.CancelNotificationReceipt(context.Background(), 42)
	if err != nil || r.Status != ReceiptStatusCanceled || r.CancelReason != ReceiptCancelAPI || r.CanceledAt == nil {
		t.Fatalf("cancel = %+v, %v", r, err)
	}
	n, err := c.CancelNotificationReceiptsByTag(context.Background(), "nas-1")
	if err != nil || n != 3 {
		t.Fatalf("cancel by tag = %d, %v", n, err)
	}
	want := []string{
		"POST /notifications/receipts/42/cancel ",
		`POST /notifications/receipts/cancel {"tag":"nas-1"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestNotificationReceipt_ErrorsAreTyped(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch {
		case r.URL.Query().Has("wait"):
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"status":429,"code":"answer_wait.limit_exceeded","retry_after_ms":2000}`))
		case r.Method == http.MethodPost && r.URL.Path == "/notifications":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"status":409,"code":"notification_receipt.limit_exceeded","detail":"25 acknowledged notifications are already active"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":404,"code":"notification_receipt.not_found"}`))
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "hlk_test")

	for name, tc := range map[string]struct {
		call func() error
		code string
	}{
		"not found":        {func() error { _, err := c.GetNotificationReceipt(context.Background(), 9, 0); return err }, ErrCodeNotificationReceiptNotFound},
		"cancel not found": {func() error { _, err := c.CancelNotificationReceipt(context.Background(), 9); return err }, ErrCodeNotificationReceiptNotFound},
		"wait limit":       {func() error { _, err := c.GetNotificationReceipt(context.Background(), 9, 20*time.Second); return err }, ErrCodeAnswerWaitLimit},
		"active cap": {func() error {
			return c.SendNotification(context.Background(), SendNotificationRequest{Title: "t", Body: "b", Acknowledge: &NotificationAcknowledge{}})
		}, ErrCodeNotificationReceiptLimit},
	} {
		calls.Store(0)
		err := tc.call()
		var he *HTTPError
		if !errors.As(err, &he) || he.Code != tc.code {
			t.Errorf("%s: err = %v, want *HTTPError %s", name, err, tc.code)
		}
		if calls.Load() != 1 {
			t.Errorf("%s: %d calls, want 1 (not retried)", name, calls.Load())
		}
	}
}
