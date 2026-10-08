package ack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overrides"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

const testKey = "hlk_ack_test"

// requestCtx is the context a handler sees for a request with query, for
// provider "acktest".
func requestCtx(t *testing.T, query string) context.Context {
	t.Helper()
	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	ov, err := overrides.Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	ctx := context.WithValue(context.Background(), overrides.ContextKey(), ov)
	return metrics.WithProvider(ctx, "acktest")
}

// logBuffer is a logger whose output the test can read.
func logBuffer() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func alert() pushward.SendNotificationRequest {
	return pushward.SendNotificationRequest{
		Title:      "Disk full",
		Body:       "nas-01",
		CollapseID: "gatus-0123456789ab",
		Level:      pushward.LevelActive,
		Push:       pushward.BoolPtr(true),
	}
}

func sent(t *testing.T, calls *[]testutil.APICall, mu *sync.Mutex) []pushward.SendNotificationRequest {
	t.Helper()
	var out []pushward.SendNotificationRequest
	for _, c := range testutil.CallsTo(testutil.GetCalls(calls, mu), http.MethodPost, "/notifications") {
		var n pushward.SendNotificationRequest
		testutil.UnmarshalBody(t, c.Body, &n)
		out = append(out, n)
	}
	return out
}

func TestTag(t *testing.T) {
	if got := Tag("gatus-0123456789ab"); got != "relay.gatus-0123456789ab" {
		t.Errorf("Tag(slug) = %q", got)
	}
	for _, id := range []string{
		"has space",
		"caf\u00e9",
		strings.Repeat("a", 59), // relay. + 59 = 65
		"tab\there",
	} {
		got := Tag(id)
		if !validTag(got) || !strings.HasPrefix(got, "relay.") {
			t.Errorf("Tag(%q) = %q, not a valid relay tag", id, got)
		}
		if got != Tag(id) {
			t.Errorf("Tag(%q) is not deterministic", id)
		}
	}
	if Tag("has space") == Tag("has  space") {
		t.Error("two identities hashed to one tag")
	}
	// 58 characters still fit as they are: relay. + 58 = 64.
	if id := strings.Repeat("a", 58); Tag(id) != "relay."+id {
		t.Errorf("Tag of a 58-character identity = %q, want it unhashed", Tag(id))
	}
}

func TestSendWithoutAckIsUnchanged(t *testing.T) {
	passive := alert()
	passive.Level = pushward.LevelPassive
	inboxOnly := alert()
	inboxOnly.Push = pushward.BoolPtr(false)
	tests := []struct {
		name, query, identity string
		req                   pushward.SendNotificationRequest
	}{
		{"no query", "", "gatus-0123456789ab", alert()},
		{"ack=0", "ack=0", "gatus-0123456789ab", alert()},
		{"no identity", "ack=1", "", alert()},
		{"passive", "ack=1", "gatus-0123456789ab", passive},
		{"push false", "ack=1", "gatus-0123456789ab", inboxOnly},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, calls, mu := testutil.MockPushWardServer(t)
			log, _ := logBuffer()
			if err := Send(requestCtx(t, tt.query), pushward.NewClient(srv.URL, testKey), log, tt.req, tt.identity); err != nil {
				t.Fatal(err)
			}
			ns := sent(t, calls, mu)
			if len(ns) != 1 {
				t.Fatalf("%d sends, want 1", len(ns))
			}
			if ns[0].Acknowledge != nil || len(ns[0].Tags) != 0 || ns[0].CollapseID != tt.req.CollapseID {
				t.Errorf("sent %+v, want it as it came in", ns[0])
			}
		})
	}
}

func TestWanted(t *testing.T) {
	passive := alert()
	passive.Level = pushward.LevelPassive
	inboxOnly := alert()
	inboxOnly.Push = pushward.BoolPtr(false)
	noLevel := alert()
	noLevel.Level, noLevel.Push = "", nil
	for _, tt := range []struct {
		name, query string
		req         pushward.SendNotificationRequest
		want        bool
	}{
		{"asked", "ack=1", alert(), true},
		{"server default level and push", "ack=1", noLevel, true},
		{"not asked", "", alert(), false},
		{"passive", "ack=1", passive, false},
		{"push false", "ack=1", inboxOnly, false},
	} {
		if got := Wanted(requestCtx(t, tt.query), tt.req); got != tt.want {
			t.Errorf("%s: Wanted = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSendAcked(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	c := pushward.NewClient(srv.URL, testKey)
	log, _ := logBuffer()
	ctx := requestCtx(t, "ack=1&ack_repeat=30&ack_expire=600")

	if err := Send(ctx, c, log, alert(), "gatus-0123456789ab"); err != nil {
		t.Fatal(err)
	}
	// No collapse id of its own, and no level: the server default alerts.
	bare := alert()
	bare.CollapseID, bare.Level = "", ""
	if err := Send(ctx, c, log, bare, "u-am-0123456789ab"); err != nil {
		t.Fatal(err)
	}

	ns := sent(t, calls, mu)
	if len(ns) != 2 {
		t.Fatalf("%d sends, want 2", len(ns))
	}
	if a := ns[0].Acknowledge; a == nil || a.RepeatSeconds != 30 || a.ExpireSeconds != 600 {
		t.Errorf("acknowledge = %+v, want repeat 30 expire 600", a)
	}
	if len(ns[0].Tags) != 1 || ns[0].Tags[0] != "relay.gatus-0123456789ab" {
		t.Errorf("tags = %q", ns[0].Tags)
	}
	if ns[0].CollapseID != "gatus-0123456789ab" {
		t.Errorf("collapse id = %q, want the alert's own", ns[0].CollapseID)
	}
	if ns[1].Acknowledge == nil || ns[1].CollapseID != "relay.u-am-0123456789ab" {
		t.Errorf("an alert without a collapse id went out as %+v, want acknowledged with the tag as collapse id", ns[1])
	}
}

// Every refusal of the acknowledge itself resends the alert once without it.
func TestSendFallsBackOnRefusal(t *testing.T) {
	tests := []struct {
		status int
		code   string
		reason string
	}{
		{http.StatusConflict, pushward.ErrCodeNotificationReceiptLimit, reasonReceiptLimit},
		{http.StatusUnprocessableEntity, pushward.ErrCodeNotificationReceiptDisabled, reasonDisabled},
		{http.StatusUnprocessableEntity, pushward.ErrCodeNotificationAnswerURLUnavailable, reasonAnswerURL},
		{http.StatusBadRequest, pushward.ErrCodeNotificationInvalid, reasonInvalid},
		{http.StatusUnprocessableEntity, "", reasonSchema},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d %s", tt.status, tt.code), func(t *testing.T) {
			srv, calls, mu := testutil.MockPushWardServerRejectingAck(t, tt.status, tt.code)
			log, buf := logBuffer()
			counter := metrics.AckFallbackTotal.WithLabelValues("acktest", tt.reason)
			before := promtest.ToFloat64(counter)

			if err := Send(requestCtx(t, "ack=1"), pushward.NewClient(srv.URL, testKey), log, alert(), "gatus-0123456789ab"); err != nil {
				t.Fatalf("Send = %v, want the plain resend to succeed", err)
			}
			ns := sent(t, calls, mu)
			if len(ns) != 2 || ns[0].Acknowledge == nil || ns[1].Acknowledge != nil || len(ns[1].Tags) != 0 {
				t.Fatalf("sends = %+v, want one acknowledged then one plain", ns)
			}
			if ns[1].CollapseID != alert().CollapseID {
				t.Errorf("resend collapse id = %q", ns[1].CollapseID)
			}
			if got := promtest.ToFloat64(counter) - before; got != 1 {
				t.Errorf("ack_fallback_total{reason=%q} grew by %v, want 1", tt.reason, got)
			}
			out := buf.String()
			if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "reason="+tt.reason) {
				t.Errorf("log = %q, want a warning naming the reason", out)
			}
			if strings.Contains(out, "hlk_") {
				t.Errorf("log carries the key: %q", out)
			}
		})
	}
}

// Anything but a refusal of the acknowledge is returned as it is: the plain
// send would fail the same way, or the server may have taken the first.
func TestSendDoesNotResend(t *testing.T) {
	tests := []struct {
		status int
		code   string
	}{
		{http.StatusUnauthorized, ""},
		{http.StatusForbidden, ""},
		{http.StatusTooManyRequests, pushward.ErrCodeRateLimitExceeded},
		{http.StatusInternalServerError, ""},
		{http.StatusConflict, ""},
		{http.StatusUnprocessableEntity, pushward.ErrCodeNotificationActivityNotFound},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d %s", tt.status, tt.code), func(t *testing.T) {
			srv, calls, mu := testutil.MockPushWardServerWith(t, testutil.MockOptions{AckStatus: tt.status, AckCode: tt.code})
			// The budget stops the client's own retries of a 429 or 5xx
			// after the first attempt.
			c := pushward.NewClient(srv.URL, testKey, pushward.WithRetryBudget(time.Millisecond))
			log, _ := logBuffer()
			err := Send(requestCtx(t, "ack=1"), c, log, alert(), "gatus-0123456789ab")
			var he *pushward.HTTPError
			if !errors.As(err, &he) || he.StatusCode != tt.status {
				t.Fatalf("Send = %v, want the %d", err, tt.status)
			}
			if ns := sent(t, calls, mu); len(ns) != 1 {
				t.Errorf("%d sends, want only the acknowledged one", len(ns))
			}
		})
	}

	t.Run("network error", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}))
		t.Cleanup(srv.Close)
		c := pushward.NewClient(srv.URL, testKey, pushward.WithRetryBudget(time.Millisecond))
		log, _ := logBuffer()
		if err := Send(requestCtx(t, "ack=1"), c, log, alert(), "gatus-0123456789ab"); err == nil {
			t.Fatal("Send = nil, want the network error")
		}
		if n := hits.Load(); n != 1 {
			t.Errorf("%d requests, want 1", n)
		}
	})
}

func TestRefusal(t *testing.T) {
	limit := &pushward.HTTPError{StatusCode: http.StatusConflict, Code: pushward.ErrCodeNotificationReceiptLimit}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"wrapped limit", fmt.Errorf("send: %w", limit), reasonReceiptLimit},
		{"open breaker", pushward.ErrCircuitOpen, ""},
		{"retries exhausted on a 5xx", fmt.Errorf("max retries exceeded: %w", &pushward.HTTPError{StatusCode: http.StatusBadGateway}), ""},
		{"quota", &pushward.QuotaExceededError{HTTPError: &pushward.HTTPError{StatusCode: http.StatusTooManyRequests, Code: pushward.ErrCodeQuotaExceeded}}, ""},
		{"400 without the code", &pushward.HTTPError{StatusCode: http.StatusBadRequest}, ""},
		{"limit code on another status", &pushward.HTTPError{StatusCode: http.StatusUnprocessableEntity, Code: pushward.ErrCodeNotificationReceiptLimit}, ""},
		{"context deadline", context.DeadlineExceeded, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := refusal(tt.err); got != tt.want {
				t.Errorf("refusal = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCancel(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	c := pushward.NewClient(srv.URL, testKey)
	log, buf := logBuffer()
	ctx := requestCtx(t, "ack=1")
	if err := Send(ctx, c, log, alert(), "gatus-0123456789ab"); err != nil {
		t.Fatal(err)
	}

	Cancel(ctx, c, log, "gatus-0123456789ab")

	cancels := testutil.CallsTo(testutil.GetCalls(calls, mu), http.MethodPost, "/notifications/receipts/cancel")
	if len(cancels) != 1 || !strings.Contains(string(cancels[0].Body), `"tag":"relay.gatus-0123456789ab"`) {
		t.Fatalf("cancel calls = %+v", cancels)
	}
	if !strings.Contains(buf.String(), "canceled=1") {
		t.Errorf("log = %q, want canceled=1", buf.String())
	}
}

func TestCancelOnlyWhenAsked(t *testing.T) {
	for _, tt := range []struct{ name, query, identity string }{
		{"no query", "", "gatus-0123456789ab"},
		{"channels=activity", "ack=1&channels=activity", "gatus-0123456789ab"},
		{"no identity", "ack=1", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, calls, mu := testutil.MockPushWardServer(t)
			log, _ := logBuffer()
			Cancel(requestCtx(t, tt.query), pushward.NewClient(srv.URL, testKey), log, tt.identity)
			if n := len(testutil.GetCalls(calls, mu)); n != 0 {
				t.Errorf("%d calls, want none", n)
			}
		})
	}
}

// A failing cancel is logged and nothing more.
func TestCancelFailureIsLogged(t *testing.T) {
	srv, _, _ := testutil.MockPushWardServerWith(t, testutil.MockOptions{CancelStatus: http.StatusBadRequest})
	log, buf := logBuffer()
	Cancel(requestCtx(t, "ack=1"), pushward.NewClient(srv.URL, testKey), log, "gatus-0123456789ab")
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "tag=relay.gatus-0123456789ab") {
		t.Errorf("log = %q, want a warning naming the tag", out)
	}
	if strings.Contains(out, "hlk_") {
		t.Errorf("log carries the key: %q", out)
	}
}

// A server that never answers holds the resolve for cancelTimeout at most.
func TestCancelIsBounded(t *testing.T) {
	old := cancelTimeout
	cancelTimeout = 50 * time.Millisecond
	t.Cleanup(func() { cancelTimeout = old })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	log, buf := logBuffer()
	start := time.Now()
	Cancel(requestCtx(t, "ack=1"), pushward.NewClient(srv.URL, testKey), log, "gatus-0123456789ab")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Cancel took %s", d)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("log = %q, want a warning", buf.String())
	}
}
