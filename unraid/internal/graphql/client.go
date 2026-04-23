package graphql

import (
	"context"
	"encoding/json"
	"fmt"
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

// dropLogEvery throttles "channel full, dropping update" warnings to at
// most one per N drops so a slow consumer can't spam the logs.
const dropLogEvery = 100

type ArrayStatus struct {
	State       string      `json:"state"`
	ParityCheck ParityCheck `json:"parityCheckStatus"`
}

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

type Notification struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Importance  string `json:"importance"`
}

// Client connects to Unraid's GraphQL API via WebSocket.
type Client struct {
	host   string
	port   int
	apiKey string
	useTLS bool

	arrayDrops *syncx.DropCounter
	notifDrops *syncx.DropCounter
}

// NewClient creates a new Unraid GraphQL client.
func NewClient(host string, port int, apiKey string, useTLS bool) *Client {
	return &Client{
		host:       host,
		port:       port,
		apiKey:     apiKey,
		useTLS:     useTLS,
		arrayDrops: syncx.NewDropCounter(dropLogEvery),
		notifDrops: syncx.NewDropCounter(dropLogEvery),
	}
}

// SubscribeArray subscribes to array status changes.
// Sends updates on the returned channel. Blocks until ctx is cancelled.
func (c *Client) SubscribeArray(ctx context.Context, ch chan<- ArrayStatus) error {
	query := `subscription { arraySubscription { state parityCheckStatus { status progress } } }`
	return c.subscribe(ctx, query, func(data json.RawMessage) {
		var wrapper struct {
			ArraySubscription ArrayStatus `json:"arraySubscription"`
		}
		if err := json.Unmarshal(data, &wrapper); err != nil {
			slog.Error("failed to decode array status", "error", err)
			return
		}
		select {
		case ch <- wrapper.ArraySubscription:
		default:
			if total, log := c.arrayDrops.Drop(); log {
				slog.Warn("array status channel full, dropping update", "total_drops", total)
			}
		}
	})
}

// SubscribeNotifications subscribes to new notifications.
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
	scheme := "ws"
	if c.useTLS {
		scheme = "wss"
	}
	url := fmt.Sprintf("%s://%s:%d/graphql", scheme, c.host, c.port)

	dialCtx, dialCancel := context.WithTimeout(ctx, dialTimeout)
	conn, _, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{
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
		case "error":
			return fmt.Errorf("subscription error: %s", string(msg.Payload))
		case "complete":
			return fmt.Errorf("subscription completed")
		}
	}
}
