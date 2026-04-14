package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

// ArrayStatus represents the current state of the Unraid array.
type ArrayStatus struct {
	State       string       `json:"state"`
	ParityCheck *ParityCheck `json:"parityCheck"`
	Disks       []Disk       `json:"disks"`
}

// ParityCheck represents an active parity check operation.
type ParityCheck struct {
	Progress float64 `json:"progress"`
	ETA      string  `json:"eta"`
}

// Disk represents a single disk in the array.
type Disk struct {
	Idx    int    `json:"idx"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Temp   *int   `json:"temp"`
}

// Notification represents an Unraid notification event.
type Notification struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Importance  string `json:"importance"`
	Timestamp   int64  `json:"timestamp"`
	Event       string `json:"event"`
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
	query := `subscription { arraySubscription { state parityCheck { progress eta } disks { idx name status temp } } }`
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
	query := `subscription { notificationAdded { id subject description importance timestamp event } }`
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

// subscribe connects to the GraphQL WebSocket and runs a subscription.
func (c *Client) subscribe(ctx context.Context, query string, handler func(json.RawMessage)) error {
	for {
		if err := c.runSubscription(ctx, query, handler); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("subscription error, reconnecting", "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
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
			"Authorization": []string{"Bearer " + c.apiKey},
		},
		Subprotocols: []string{"graphql-transport-ws"},
	})
	dialCancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()

	// Connection init
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "connection_init"}); err != nil {
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
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				slog.Error("failed to decode payload", "error", err)
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
