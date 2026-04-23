package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/mac-lucky/pushward-integrations/shared/syncx"
)

// dialTimeout bounds the WebSocket handshake. The read loop uses the
// caller's ctx (no timeout) since subscriptions are long-lived.
const dialTimeout = 30 * time.Second

// queryTimeout bounds a single HTTP GraphQL query.
const queryTimeout = 10 * time.Second

// dropLogEvery throttles "channel full, dropping update" warnings to at
// most one per N drops so a slow consumer can't spam the logs.
const dropLogEvery = 100

// ArrayState mirrors Unraid's ArrayState enum. SDL values are uppercase;
// intermediate STARTING/STOPPING states seen in older docs do not exist.
type ArrayState string

const (
	ArrayStateStarted             ArrayState = "STARTED"
	ArrayStateStopped             ArrayState = "STOPPED"
	ArrayStateNewArray            ArrayState = "NEW_ARRAY"
	ArrayStateReconDisk           ArrayState = "RECON_DISK"
	ArrayStateDisableDisk         ArrayState = "DISABLE_DISK"
	ArrayStateSwapDsbl            ArrayState = "SWAP_DSBL"
	ArrayStateInvalidExpansion    ArrayState = "INVALID_EXPANSION"
	ArrayStateParityNotBiggest    ArrayState = "PARITY_NOT_BIGGEST"
	ArrayStateTooManyMissingDisks ArrayState = "TOO_MANY_MISSING_DISKS"
	ArrayStateNewDiskTooSmall     ArrayState = "NEW_DISK_TOO_SMALL"
	ArrayStateNoDataDisks         ArrayState = "NO_DATA_DISKS"
)

type ArrayStatus struct {
	State       ArrayState   `json:"state"`
	ParityCheck *ParityCheck `json:"parityCheckStatus"`
}

// ParityStatus mirrors Unraid's ParityCheckStatus enum. The TypeScript
// source declares the values lowercase (never_run, running, ...), and
// TypeGraphQL serializes string-enum values (not keys) to the wire.
type ParityStatus string

const (
	ParityStatusNeverRun  ParityStatus = "never_run"
	ParityStatusRunning   ParityStatus = "running"
	ParityStatusPaused    ParityStatus = "paused"
	ParityStatusCompleted ParityStatus = "completed"
	ParityStatusCancelled ParityStatus = "cancelled"
	ParityStatusFailed    ParityStatus = "failed"
)

type ParityCheck struct {
	Status   ParityStatus `json:"status"`
	Progress float64      `json:"progress"`
}

func (p ParityCheck) IsActive() bool {
	return p.Status == ParityStatusRunning || p.Status == ParityStatusPaused
}

// Importance mirrors Unraid's NotificationImportance enum. Values are
// UPPERCASE in the SDL (ALERT/INFO/WARNING); comparing against lowercase
// silently downgrades every alert, so don't.
type Importance string

const (
	ImportanceAlert   Importance = "ALERT"
	ImportanceWarning Importance = "WARNING"
	ImportanceInfo    Importance = "INFO"
)

type Notification struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Subject     string     `json:"subject"`
	Description string     `json:"description"`
	Importance  Importance `json:"importance"`
}

// Client connects to Unraid's GraphQL API. Array state is fetched by
// polling `query { array { ... } }` because `subscription arraySubscription`
// fails with "Cannot return null for non-nullable field" on the server
// (a schema bug in Unraid >=v4.x). Notifications still use the working
// subscription.
type Client struct {
	apiKey     string
	httpURL    string
	wsURL      string
	httpClient *http.Client

	notifDrops *syncx.DropCounter
}

// queryBody is the precomputed JSON body for the array poll — the query
// is constant, so marshaling once at package load avoids per-poll work.
var queryBody = []byte(`{"query":"query { array { state parityCheckStatus { status progress } } }"}`)

// NewClient creates a new Unraid GraphQL client.
func NewClient(host string, port int, apiKey string, useTLS bool) *Client {
	httpScheme, wsScheme := "http", "ws"
	if useTLS {
		httpScheme, wsScheme = "https", "wss"
	}
	return &Client{
		apiKey:     apiKey,
		httpURL:    fmt.Sprintf("%s://%s:%d/graphql", httpScheme, host, port),
		wsURL:      fmt.Sprintf("%s://%s:%d/graphql", wsScheme, host, port),
		httpClient: &http.Client{Timeout: queryTimeout},
		notifDrops: syncx.NewDropCounter(dropLogEvery),
	}
}

// graphQLResponse is the standard GraphQL over-the-wire envelope.
type graphQLResponse struct {
	Data   json.RawMessage   `json:"data"`
	Errors []json.RawMessage `json:"errors"`
}

// QueryArray fetches current array state via `query { array { ... } }`.
// The subscription form is broken on Unraid v4.x so callers must poll.
func (c *Client) QueryArray(ctx context.Context) (*ArrayStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.httpURL, bytes.NewReader(queryBody))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(respBody))
	}

	var env graphQLResponse
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("decode envelope: %w (body: %s)", err, string(respBody))
	}
	if len(env.Errors) > 0 {
		return nil, fmt.Errorf("graphql errors: %s", env.Errors)
	}

	var payload struct {
		Array *ArrayStatus `json:"array"`
	}
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		return nil, fmt.Errorf("decode data: %w", err)
	}
	if payload.Array == nil {
		return nil, fmt.Errorf("array field missing from response")
	}
	return payload.Array, nil
}

// SubscribeNotifications subscribes to new notifications. Blocks until
// ctx is cancelled, reconnecting with exponential backoff on error.
func (c *Client) SubscribeNotifications(ctx context.Context, ch chan<- Notification) error {
	query := `subscription { notificationAdded { id title subject description importance } }`
	return c.subscribe(ctx, query, func(data json.RawMessage) {
		var wrapper struct {
			NotificationAdded Notification `json:"notificationAdded"`
		}
		if err := json.Unmarshal(data, &wrapper); err != nil {
			slog.Error("failed to decode notification", "error", err)
			return
		}
		select {
		case ch <- wrapper.NotificationAdded:
		default:
			if total, log := c.notifDrops.Drop(); log {
				slog.Warn("notification channel full, dropping update", "total_drops", total)
			}
		}
	})
}

func (c *Client) subscribe(ctx context.Context, query string, handler func(json.RawMessage)) error {
	attempt := 0
	for {
		if err := c.runSubscription(ctx, query, handler); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			attempt++
			base := min(time.Second<<(attempt-1), 60*time.Second)
			backoff := base/2 + rand.N(base/2) // #nosec G404 -- jitter for retry backoff, not security-sensitive
			slog.Error("subscription error, reconnecting", "error", err, "attempt", attempt, "backoff", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			continue
		}
		attempt = 0
	}
}

func (c *Client) runSubscription(ctx context.Context, query string, handler func(json.RawMessage)) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, dialTimeout)
	conn, _, err := websocket.Dial(dialCtx, c.wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Api-Key": []string{c.apiKey},
		},
		Subprotocols: []string{"graphql-transport-ws"},
	})
	dialCancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()

	// NestJS does not lowercase connectionParams keys, so the header name
	// here must match the passport-http-header-strategy lookup (x-api-key).
	init := map[string]any{
		"type":    "connection_init",
		"payload": map[string]any{"x-api-key": c.apiKey},
	}
	if err := wsjson.Write(ctx, conn, init); err != nil {
		return fmt.Errorf("connection_init: %w", err)
	}

	// Wait for connection_ack
	var ack map[string]any
	if err := wsjson.Read(ctx, conn, &ack); err != nil {
		return fmt.Errorf("reading ack: %w", err)
	}
	if ack["type"] != "connection_ack" {
		return fmt.Errorf("expected connection_ack, got %v", ack["type"])
	}

	// Subscribe
	subMsg := map[string]any{
		"id":   "1",
		"type": "subscribe",
		"payload": map[string]any{
			"query": query,
		},
	}
	if err := wsjson.Write(ctx, conn, subMsg); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	// Read messages
	for {
		var msg struct {
			Type    string          `json:"type"`
			ID      string          `json:"id"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return fmt.Errorf("reading message: %w", err)
		}

		switch msg.Type {
		case "next":
			var payload struct {
				Data   json.RawMessage   `json:"data"`
				Errors []json.RawMessage `json:"errors"`
			}
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				slog.Error("failed to decode payload", "error", err, "raw", string(msg.Payload))
				continue
			}
			if len(payload.Errors) > 0 {
				slog.Error("subscription payload errors", "errors", payload.Errors, "raw", string(msg.Payload))
				continue
			}
			if len(payload.Data) == 0 || string(payload.Data) == "null" {
				slog.Warn("subscription payload had no data", "raw", string(msg.Payload))
				continue
			}
			handler(payload.Data)
		case "ping":
			// graphql-transport-ws requires a pong in response ASAP;
			// ignoring pings makes compliant servers close the socket.
			pong := map[string]any{"type": "pong"}
			if len(msg.Payload) > 0 {
				pong["payload"] = msg.Payload
			}
			if err := wsjson.Write(ctx, conn, pong); err != nil {
				return fmt.Errorf("pong: %w", err)
			}
		case "pong":
			// Server's answer to a client ping — we don't send any, but
			// tolerate receipt rather than treating as an unknown type.
		case "error":
			return fmt.Errorf("subscription error: %s", string(msg.Payload))
		case "complete":
			return fmt.Errorf("subscription completed")
		}
	}
}
