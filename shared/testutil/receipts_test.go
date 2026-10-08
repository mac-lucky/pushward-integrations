package testutil_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// postProblem posts body to path and returns the status and the Problem code.
func postProblem(t *testing.T, url, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var p struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&p)
	return resp.StatusCode, p.Code
}

func httpError(t *testing.T, err error) *pushward.HTTPError {
	t.Helper()
	var he *pushward.HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want a *pushward.HTTPError", err)
	}
	return he
}

func quoted(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ",")
}

// The server's acknowledge rules: what its schema bounds is a 422 with no
// code, checked first; what its handler checks is a 400 notification.invalid.
func TestMockAcknowledgeBounds(t *testing.T) {
	urlActions := func(n int) string {
		a := make([]string, n)
		for i := range a {
			a[i] = fmt.Sprintf(`{"id":"a%d","title":"Open","url":"https://example.com/%d"}`, i, i)
		}
		return strings.Join(a, ",")
	}
	tags := func(n int) string {
		s := make([]string, n)
		for i := range s {
			s[i] = fmt.Sprintf("tag-%d", i)
		}
		return quoted(s)
	}
	for _, tt := range []struct {
		name   string
		ack    string
		rest   string
		ok     bool
		schema bool
	}{
		{name: "defaults", ack: `{}`, ok: true},
		{name: "repeat 29", ack: `{"repeat_seconds":29}`, schema: true},
		{name: "repeat 30", ack: `{"repeat_seconds":30}`, ok: true},
		{name: "repeat 3600", ack: `{"repeat_seconds":3600}`, ok: true},
		{name: "repeat 3601", ack: `{"repeat_seconds":3601}`, schema: true},
		{name: "expire 59", ack: `{"expire_seconds":59}`, schema: true},
		{name: "expire 60", ack: `{"expire_seconds":60}`, ok: true},
		{name: "expire 10800", ack: `{"expire_seconds":10800}`, ok: true},
		{name: "expire 10801", ack: `{"expire_seconds":10801}`, schema: true},
		{name: "action title 64", ack: fmt.Sprintf(`{"action_title":%q}`, strings.Repeat("a", 64)), ok: true},
		{name: "action title 65", ack: fmt.Sprintf(`{"action_title":%q}`, strings.Repeat("a", 65)), schema: true},
		{name: "10 tags", ack: `{}`, rest: `,"tags":[` + tags(10) + `]`, ok: true},
		{name: "11 tags", ack: `{}`, rest: `,"tags":[` + tags(11) + `]`, schema: true},
		{name: "repeated tags count once", ack: `{}`, rest: `,"tags":["a","a"]`, ok: true},
		{name: "64-character tag", ack: `{}`, rest: fmt.Sprintf(`,"tags":[%q]`, strings.Repeat("t", 64)), ok: true},
		{name: "65-character tag", ack: `{}`, rest: fmt.Sprintf(`,"tags":[%q]`, strings.Repeat("t", 65))},
		{name: "empty tag", ack: `{}`, rest: `,"tags":[""]`},
		{name: "tag with a space", ack: `{}`, rest: `,"tags":["disk full"]`},
		{name: "non-ASCII tag", ack: `{}`, rest: `,"tags":["caf\u00e9"]`},
		{name: "pw_ack is reserved", ack: `{}`, rest: `,"actions":[{"id":"pw_ack","title":"Ack"}]`},
		{name: "9 url actions leave room", ack: `{}`, rest: `,"actions":[` + urlActions(9) + `]`, ok: true},
		{name: "10 url actions leave none", ack: `{}`, rest: `,"actions":[` + urlActions(10) + `]`},
		{name: "10 actions, one answerable", ack: `{}`, rest: `,"actions":[` + urlActions(9) + `,{"id":"ok","title":"OK"}]`, ok: true},
		{name: "passive", ack: `{}`, rest: `,"level":"passive"`},
		{name: "push false", ack: `{}`, rest: `,"push":false`},
		{name: "tags without acknowledge", ack: `null`, rest: `,"tags":["disk"]`},
		{name: "11 tags without acknowledge", ack: `null`, rest: `,"tags":[` + tags(11) + `]`, schema: true},
		{name: "schema before handler", ack: `{"repeat_seconds":10}`, rest: `,"level":"passive"`, schema: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := testutil.MockPushWardServer(t)
			status, code := postProblem(t, srv.URL, "/notifications", `{"title":"t","body":"b","acknowledge":`+tt.ack+tt.rest+`}`)
			switch {
			case tt.ok && status != http.StatusCreated:
				t.Errorf("got %d %s, want 201", status, code)
			case tt.schema && (status != http.StatusUnprocessableEntity || code != ""):
				t.Errorf("got %d %q, want 422 without a code", status, code)
			case !tt.ok && !tt.schema && (status != http.StatusBadRequest || code != pushward.ErrCodeNotificationInvalid):
				t.Errorf("got %d %q, want 400 %s", status, code, pushward.ErrCodeNotificationInvalid)
			}
		})
	}
}

func TestMockReceipts(t *testing.T) {
	ctx := context.Background()
	srv, calls, mu := testutil.MockPushWardServer(t)
	a := pushward.NewClient(srv.URL, "hlk_a")
	b := pushward.NewClient(srv.URL, "hlk_b")

	first, err := a.SendNotificationResult(ctx, pushward.SendNotificationRequest{
		Title: "Disk full", Body: "nas", CollapseID: "nas-disk",
		Acknowledge: &pushward.NotificationAcknowledge{RepeatSeconds: 300, ExpireSeconds: 600},
		Tags:        []string{"nas-disk", "nas-disk", "storage"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := first.Receipt
	if first.ID != 1 || r == nil || r.NotificationID != 1 || r.Status != pushward.ReceiptStatusActive || r.RepeatSeconds != 300 {
		t.Fatalf("first send = %+v, receipt %+v", first, r)
	}
	if got := r.ExpiresAt.Sub(r.CreatedAt); got != 10*time.Minute {
		t.Errorf("expires %v after the send, want 10m", got)
	}
	if strings.Join(r.Tags, ",") != "nas-disk,storage" {
		t.Errorf("tags = %v, want them de-duplicated", r.Tags)
	}

	plain, err := a.SendNotificationResult(ctx, pushward.SendNotificationRequest{Title: "t", Body: "b"})
	if err != nil || plain.ID != 2 || plain.Receipt != nil {
		t.Fatalf("plain send = %+v, %v", plain, err)
	}
	if _, err := a.GetNotificationReceipt(ctx, plain.ID, 0); httpError(t, err).Code != pushward.ErrCodeNotificationReceiptNotFound {
		t.Errorf("receipt of a plain send: %v", err)
	}

	// Another key sees none of it.
	if _, err := b.GetNotificationReceipt(ctx, first.ID, 0); httpError(t, err).StatusCode != http.StatusNotFound {
		t.Errorf("another key read the receipt: %v", err)
	}
	if n, err := b.CancelNotificationReceiptsByTag(ctx, "nas-disk"); err != nil || n != 0 {
		t.Errorf("another key canceled %d, %v", n, err)
	}

	// A newer acknowledged send with the same collapse id supersedes.
	second, err := a.SendNotificationResult(ctx, pushward.SendNotificationRequest{
		Title: "Disk full", Body: "nas", CollapseID: "nas-disk",
		Acknowledge: &pushward.NotificationAcknowledge{}, Tags: []string{"nas-disk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != 3 || second.Receipt.RepeatSeconds != 60 {
		t.Errorf("second send = %+v, receipt %+v; want id 3 and the 60s default", second, second.Receipt)
	}
	old, err := a.GetNotificationReceipt(ctx, first.ID, 0)
	if err != nil || old.Status != pushward.ReceiptStatusCanceled || old.CancelReason != pushward.ReceiptCancelSuperseded || old.CanceledAt == nil {
		t.Errorf("superseded receipt = %+v, %v", old, err)
	}

	if n, err := a.CancelNotificationReceiptsByTag(ctx, "nas-disk"); err != nil || n != 1 {
		t.Errorf("cancel by tag = %d, %v; want 1", n, err)
	}
	if n, err := a.CancelNotificationReceiptsByTag(ctx, "nas-disk"); err != nil || n != 0 {
		t.Errorf("second cancel by tag = %d, %v; want 0", n, err)
	}
	got, err := a.GetNotificationReceipt(ctx, second.ID, 0)
	if err != nil || got.Status != pushward.ReceiptStatusCanceled || got.CancelReason != pushward.ReceiptCancelTag {
		t.Errorf("receipt canceled by tag = %+v, %v", got, err)
	}

	// Canceling a finished receipt returns it unchanged.
	if got, err := a.CancelNotificationReceipt(ctx, first.ID); err != nil || got.CancelReason != pushward.ReceiptCancelSuperseded {
		t.Errorf("cancel of a superseded receipt = %+v, %v", got, err)
	}
	third, err := a.SendNotificationResult(ctx, pushward.SendNotificationRequest{Title: "t", Body: "b", Acknowledge: &pushward.NotificationAcknowledge{}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.CancelNotificationReceipt(ctx, third.ID); err != nil || got.Status != pushward.ReceiptStatusCanceled || got.CancelReason != pushward.ReceiptCancelAPI {
		t.Errorf("cancel by id = %+v, %v", got, err)
	}
	if _, err := a.CancelNotificationReceipt(ctx, 999); httpError(t, err).Code != pushward.ErrCodeNotificationReceiptNotFound {
		t.Errorf("cancel of an unknown id: %v", err)
	}

	// A tag the server's schema refuses is a 422 with no code.
	he := httpError(t, func() error { _, err := a.CancelNotificationReceiptsByTag(ctx, "disk full"); return err }())
	if he.StatusCode != http.StatusUnprocessableEntity || he.Code != "" {
		t.Errorf("invalid tag: %d %q, want 422 without a code", he.StatusCode, he.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if n := len(testutil.CallsTo(recorded, http.MethodPost, "/notifications")); n != 4 {
		t.Errorf("CallsTo found %d sends, want 4", n)
	}
	if n := len(testutil.CallsTo(recorded, http.MethodPost, "/notifications/receipts/cancel")); n != 4 {
		t.Errorf("CallsTo found %d tag cancels, want 4", n)
	}
}

func TestMockReceiptLimit(t *testing.T) {
	ctx := context.Background()
	srv, _, _ := testutil.MockPushWardServer(t)
	c := pushward.NewClient(srv.URL, "hlk_a")
	send := func(collapseID string) error {
		_, err := c.SendNotificationResult(ctx, pushward.SendNotificationRequest{
			Title: "t", Body: "b", CollapseID: collapseID,
			Acknowledge: &pushward.NotificationAcknowledge{}, Tags: []string{collapseID},
		})
		return err
	}
	for i := range 25 {
		if err := send(fmt.Sprintf("alert-%d", i)); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	he := httpError(t, send("alert-25"))
	if he.StatusCode != http.StatusConflict || he.Code != pushward.ErrCodeNotificationReceiptLimit {
		t.Fatalf("26th active: %d %q", he.StatusCode, he.Code)
	}
	// A resend supersedes its own predecessor, so it fits under the cap.
	if err := send("alert-3"); err != nil {
		t.Errorf("superseding send at the cap: %v", err)
	}
	// Plain sends are not capped.
	if err := c.SendNotification(ctx, pushward.SendNotificationRequest{Title: "t", Body: "b"}); err != nil {
		t.Errorf("plain send at the cap: %v", err)
	}
	if _, err := c.CancelNotificationReceiptsByTag(ctx, "alert-0"); err != nil {
		t.Fatal(err)
	}
	if err := send("alert-25"); err != nil {
		t.Errorf("send after a cancel freed a slot: %v", err)
	}
}

func TestMockRejectingAck(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		status int
		code   string
	}{
		{http.StatusConflict, pushward.ErrCodeNotificationReceiptLimit},
		{http.StatusUnprocessableEntity, pushward.ErrCodeNotificationReceiptDisabled},
		{http.StatusUnprocessableEntity, ""},
	} {
		srv, calls, mu := testutil.MockPushWardServerRejectingAck(t, tt.status, tt.code)
		c := pushward.NewClient(srv.URL, "hlk_a")
		req := pushward.SendNotificationRequest{Title: "t", Body: "b", Acknowledge: &pushward.NotificationAcknowledge{}, Tags: []string{"x"}}
		he := httpError(t, c.SendNotification(ctx, req))
		if he.StatusCode != tt.status || he.Code != tt.code {
			t.Errorf("acknowledged send: %d %q, want %d %q", he.StatusCode, he.Code, tt.status, tt.code)
		}
		req.Acknowledge, req.Tags = nil, nil
		if err := c.SendNotification(ctx, req); err != nil {
			t.Errorf("plain send after a refused acknowledge: %v", err)
		}
		if n := len(testutil.CallsTo(testutil.GetCalls(calls, mu), http.MethodPost, "/notifications")); n != 2 {
			t.Errorf("recorded %d sends, want 2", n)
		}
	}

	// The acknowledge rules are checked before the rejection, as on the server.
	srv, _, _ := testutil.MockPushWardServerRejectingAck(t, http.StatusConflict, pushward.ErrCodeNotificationReceiptLimit)
	if status, code := postProblem(t, srv.URL, "/notifications", `{"title":"t","body":"b","acknowledge":{"repeat_seconds":5}}`); status != http.StatusUnprocessableEntity || code != "" {
		t.Errorf("out-of-bounds acknowledge: %d %q", status, code)
	}
	if status, code := postProblem(t, srv.URL, "/notifications", `{"title":"t","body":"b","acknowledge":{},"push":false}`); status != http.StatusBadRequest || code != pushward.ErrCodeNotificationInvalid {
		t.Errorf("acknowledge without push: %d %q", status, code)
	}
}

func TestMockCancelStatus(t *testing.T) {
	ctx := context.Background()
	srv, _, _ := testutil.MockPushWardServerWith(t, testutil.MockOptions{CancelStatus: http.StatusForbidden})
	c := pushward.NewClient(srv.URL, "hlk_a")
	sn, err := c.SendNotificationResult(ctx, pushward.SendNotificationRequest{
		Title: "t", Body: "b", Acknowledge: &pushward.NotificationAcknowledge{}, Tags: []string{"x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CancelNotificationReceiptsByTag(ctx, "x"); httpError(t, err).StatusCode != http.StatusForbidden {
		t.Errorf("cancel by tag: %v", err)
	}
	if _, err := c.CancelNotificationReceipt(ctx, sn.ID); httpError(t, err).StatusCode != http.StatusForbidden {
		t.Errorf("cancel by id: %v", err)
	}
	if r, err := c.GetNotificationReceipt(ctx, sn.ID, 0); err != nil || r.Status != pushward.ReceiptStatusActive {
		t.Errorf("receipt after failed cancels = %+v, %v", r, err)
	}
	// The schema check still comes first.
	if _, err := c.CancelNotificationReceiptsByTag(ctx, ""); httpError(t, err).StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("empty tag: %v", err)
	}
}
