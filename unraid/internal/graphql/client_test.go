package graphql

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// --- Typed-enum guards ---

func TestImportanceConstants(t *testing.T) {
	// Unraid's SDL ships these values UPPERCASE via registerEnumType on a
	// string enum whose values are uppercase. If these drift, every alert
	// falls through to passive/info silently.
	cases := []struct {
		got, want Importance
	}{
		{ImportanceAlert, "ALERT"},
		{ImportanceWarning, "WARNING"},
		{ImportanceInfo, "INFO"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%q != %q", c.got, c.want)
		}
	}
}

func TestArrayStateConstants(t *testing.T) {
	// These must match the SDL enum members exactly; STARTING/STOPPING
	// intentionally absent — Unraid never emits them.
	cases := []struct {
		got, want ArrayState
	}{
		{ArrayStateStarted, "STARTED"},
		{ArrayStateStopped, "STOPPED"},
		{ArrayStateNewArray, "NEW_ARRAY"},
		{ArrayStateReconDisk, "RECON_DISK"},
		{ArrayStateDisableDisk, "DISABLE_DISK"},
		{ArrayStateSwapDsbl, "SWAP_DSBL"},
		{ArrayStateInvalidExpansion, "INVALID_EXPANSION"},
		{ArrayStateParityNotBiggest, "PARITY_NOT_BIGGEST"},
		{ArrayStateTooManyMissingDisks, "TOO_MANY_MISSING_DISKS"},
		{ArrayStateNewDiskTooSmall, "NEW_DISK_TOO_SMALL"},
		{ArrayStateNoDataDisks, "NO_DATA_DISKS"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%q != %q", c.got, c.want)
		}
	}
}

func TestParityCheck_IsActive(t *testing.T) {
	// Values are lowercase in the SDL (string-enum values, serialized as-is).
	cases := []struct {
		status ParityStatus
		want   bool
	}{
		{ParityStatusRunning, true},
		{ParityStatusPaused, true},
		{ParityStatusNeverRun, false},
		{ParityStatusCompleted, false},
		{ParityStatusCancelled, false},
		{ParityStatusFailed, false},
		{"", false},
		{"RUNNING", false}, // wrong casing must not count as active
	}
	for _, c := range cases {
		got := ParityCheck{Status: c.status}.IsActive()
		if got != c.want {
			t.Errorf("IsActive(%q) = %v, want %v", c.status, got, c.want)
		}
	}
}

// --- QueryArray HTTP tests ---

// newTestClient splits a test server URL so NewClient can rebuild it.
func newTestClient(t *testing.T, serverURL, apiKey string) *Client {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split host:port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return NewClient(host, port, apiKey, false)
}

func TestQueryArray_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"array":{"state":"STARTED","parityCheckStatus":{"status":"running","progress":42}}}}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, "k")
	got, err := c.QueryArray(context.Background())
	if err != nil {
		t.Fatalf("QueryArray: %v", err)
	}
	if got.State != ArrayStateStarted {
		t.Errorf("state = %q, want STARTED", got.State)
	}
	if got.ParityCheck == nil {
		t.Fatal("ParityCheck should not be nil")
	}
	if got.ParityCheck.Status != ParityStatusRunning {
		t.Errorf("parity status = %q, want running", got.ParityCheck.Status)
	}
	if got.ParityCheck.Progress != 42 {
		t.Errorf("progress = %v, want 42", got.ParityCheck.Progress)
	}
}

func TestQueryArray_NullParity(t *testing.T) {
	// parityCheckStatus is nullable in the SDL — a JSON null must decode
	// to a nil pointer, not a zero-value ParityCheck.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"array":{"state":"STARTED","parityCheckStatus":null}}}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, "k")
	got, err := c.QueryArray(context.Background())
	if err != nil {
		t.Fatalf("QueryArray: %v", err)
	}
	if got.ParityCheck != nil {
		t.Errorf("ParityCheck = %+v, want nil for JSON null", got.ParityCheck)
	}
}

func TestQueryArray_GraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"Unauthorized","path":["array"]}]}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, "k")
	_, err := c.QueryArray(context.Background())
	if err == nil {
		t.Fatal("expected error when GraphQL response has errors")
	}
	if !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("error should surface server message, got: %v", err)
	}
}

func TestQueryArray_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("bad key"))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, "k")
	_, err := c.QueryArray(context.Background())
	if err == nil {
		t.Fatal("expected error for HTTP 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status code, got: %v", err)
	}
}

func TestQueryArray_SendsAPIKeyAndQuery(t *testing.T) {
	var (
		method, path, contentType, apiKey string
		body                              []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		contentType = r.Header.Get("Content-Type")
		apiKey = r.Header.Get("X-Api-Key")
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"data":{"array":{"state":"STARTED","parityCheckStatus":null}}}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, "my-key")
	if _, err := c.QueryArray(context.Background()); err != nil {
		t.Fatalf("QueryArray: %v", err)
	}

	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	if path != "/graphql" {
		t.Errorf("path = %q, want /graphql", path)
	}
	if contentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", contentType)
	}
	if apiKey != "my-key" {
		t.Errorf("X-Api-Key = %q, want my-key", apiKey)
	}

	var req struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request body: %v (body: %s)", err, body)
	}
	// Required SDL field names; forbidden fields would either waste
	// bandwidth or be outright rejected by the schema.
	for _, want := range []string{"array", "state", "parityCheckStatus", "status", "progress"} {
		if !strings.Contains(req.Query, want) {
			t.Errorf("query missing %q: %s", want, req.Query)
		}
	}
	for _, banned := range []string{"disks", "notification", "timestamp", "eta"} {
		if strings.Contains(req.Query, banned) {
			t.Errorf("query must not include %q (unused field): %s", banned, req.Query)
		}
	}
}

func TestQueryArray_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"array":{"state":"STARTED","parityCheckStatus":null}}}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, "k")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.QueryArray(ctx); err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestQueryArray_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, "k")
	_, err := c.QueryArray(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// --- WebSocket subscription tests ---

// wsServer spins up an httptest server that upgrades to a WebSocket on
// /graphql and runs the given handler against the server-side conn.
// It returns the server URL and a cleanup func.
type recorded struct {
	subprotocol    string
	initPayload    map[string]any
	subscribeQuery string
}

func newWSServer(t *testing.T, handler func(ctx context.Context, conn *websocket.Conn, rec *recorded)) (string, *recorded) {
	t.Helper()
	rec := &recorded{}

	var mu sync.Mutex // serialize handler runs if the client reconnects

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{"graphql-transport-ws"},
		})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		rec.subprotocol = conn.Subprotocol()
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		handler(ctx, conn, rec)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, rec
}

// happyPathHandler runs the full graphql-transport-ws handshake, sends one
// notification, then "complete".
func happyPathHandler(t *testing.T, notif Notification) func(context.Context, *websocket.Conn, *recorded) {
	t.Helper()
	return func(ctx context.Context, conn *websocket.Conn, rec *recorded) {
		var init struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if err := wsjson.Read(ctx, conn, &init); err != nil {
			t.Errorf("read init: %v", err)
			return
		}
		if init.Type != "connection_init" {
			t.Errorf("want connection_init, got %q", init.Type)
		}
		rec.initPayload = init.Payload

		if err := wsjson.Write(ctx, conn, map[string]any{"type": "connection_ack"}); err != nil {
			t.Errorf("write ack: %v", err)
			return
		}

		var sub struct {
			ID      string         `json:"id"`
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if err := wsjson.Read(ctx, conn, &sub); err != nil {
			t.Errorf("read subscribe: %v", err)
			return
		}
		if sub.Type != "subscribe" {
			t.Errorf("want subscribe, got %q", sub.Type)
		}
		if q, _ := sub.Payload["query"].(string); q != "" {
			rec.subscribeQuery = q
		}

		next := map[string]any{
			"id":   sub.ID,
			"type": "next",
			"payload": map[string]any{
				"data": map[string]any{
					"notificationAdded": notif,
				},
			},
		}
		if err := wsjson.Write(ctx, conn, next); err != nil {
			t.Errorf("write next: %v", err)
			return
		}

		_ = wsjson.Write(ctx, conn, map[string]any{"id": sub.ID, "type": "complete"})
	}
}

func TestSubscribeNotifications_HandshakeAndMessage(t *testing.T) {
	want := Notification{
		ID:          "n1",
		Title:       "Disk alert",
		Subject:     "SMART error",
		Description: "reallocated sectors",
		Importance:  ImportanceAlert,
	}
	srvURL, rec := newWSServer(t, happyPathHandler(t, want))

	c := newTestClient(t, srvURL, "secret-key")
	ch := make(chan Notification, 1)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.SubscribeNotifications(ctx, ch)
	}()

	select {
	case got := <-ch:
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for notification")
	}

	cancel()
	<-done

	if rec.subprotocol != "graphql-transport-ws" {
		t.Errorf("subprotocol = %q, want graphql-transport-ws", rec.subprotocol)
	}
	// NestJS's Apollo integration does not lowercase connectionParams keys.
	// The passport strategy reads headers['x-api-key'] (lowercase), so the
	// key here must be exactly that string.
	if _, ok := rec.initPayload["x-api-key"]; !ok {
		t.Errorf("connection_init payload missing lowercase x-api-key; payload keys = %v", keys(rec.initPayload))
	}
	if v, _ := rec.initPayload["x-api-key"].(string); v != "secret-key" {
		t.Errorf("x-api-key = %q, want secret-key", v)
	}
	if !strings.Contains(rec.subscribeQuery, "notificationAdded") {
		t.Errorf("subscribe query should include notificationAdded: %s", rec.subscribeQuery)
	}
}

// Server-sent {type:"error",...} must surface as a Go error so the
// reconnect loop can log it — otherwise we get silent subscription death.
func TestSubscribeNotifications_ServerErrorFrame(t *testing.T) {
	srvURL, _ := newWSServer(t, func(ctx context.Context, conn *websocket.Conn, rec *recorded) {
		var init map[string]any
		_ = wsjson.Read(ctx, conn, &init)
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "connection_ack"})
		var sub struct {
			ID string `json:"id"`
		}
		_ = wsjson.Read(ctx, conn, &sub)
		_ = wsjson.Write(ctx, conn, map[string]any{
			"id":      sub.ID,
			"type":    "error",
			"payload": []map[string]any{{"message": "bad query"}},
		})
	})

	c := newTestClient(t, srvURL, "k")
	ch := make(chan Notification, 1)
	// 50ms is enough: the first backoff is 500ms-1s, so the backoff select
	// picks up ctx.Done() well before the next reconnect. If this hangs
	// the client is silently swallowing the error frame.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := c.SubscribeNotifications(ctx, ch)
	if err == nil {
		t.Fatal("expected error after context timeout")
	}
}

// The graphql-transport-ws spec requires pong ASAP in response to ping;
// silent clients get closed with code 4400 by compliant servers.
func TestSubscribeNotifications_RespondsToPing(t *testing.T) {
	pongReceived := make(chan map[string]any, 1)
	srvURL, _ := newWSServer(t, func(ctx context.Context, conn *websocket.Conn, rec *recorded) {
		var init map[string]any
		_ = wsjson.Read(ctx, conn, &init)
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "connection_ack"})
		var sub map[string]any
		_ = wsjson.Read(ctx, conn, &sub)
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "ping"})
		var pong map[string]any
		if err := wsjson.Read(ctx, conn, &pong); err == nil {
			pongReceived <- pong
		}
	})

	c := newTestClient(t, srvURL, "k")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.SubscribeNotifications(ctx, make(chan Notification, 1)) }()

	select {
	case msg := <-pongReceived:
		if msg["type"] != "pong" {
			t.Errorf("expected pong, got %v", msg["type"])
		}
	case <-time.After(time.Second):
		t.Fatal("client did not respond to ping")
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
