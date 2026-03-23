package bambulab

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Client connects to a BambuLab printer via MQTT and delivers print status updates.
type Client struct {
	host               string
	accessCode         string
	serial             string
	insecureSkipVerify bool

	mqttClient mqtt.Client
	mu         sync.Mutex
	state      MergedState
	updateCh   chan struct{} // signals new data available
}

// NewClient creates a new BambuLab MQTT client.
func NewClient(host, accessCode, serial string, insecureSkipVerify bool) *Client {
	return &Client{
		host:               host,
		accessCode:         accessCode,
		serial:             serial,
		insecureSkipVerify: insecureSkipVerify,
		updateCh:           make(chan struct{}, 1),
	}
}

// Connect establishes the MQTT connection and subscribes to the report topic.
func (c *Client) Connect() error {
	broker := fmt.Sprintf("tls://%s:8883", c.host)
	topic := fmt.Sprintf("device/%s/report", c.serial)

	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(fmt.Sprintf("pushward-bambulab-%d", time.Now().UnixMilli())).
		SetUsername("bblp").
		SetPassword(c.accessCode).
		SetKeepAlive(60 * time.Second).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30 * time.Second).
		SetTLSConfig(&tls.Config{InsecureSkipVerify: c.insecureSkipVerify}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			slog.Warn("MQTT connection lost", "error", err)
		}).
		SetOnConnectHandler(func(client mqtt.Client) {
			slog.Info("MQTT connected, subscribing", "topic", topic)
			if token := client.Subscribe(topic, 0, c.onMessage); token.Wait() && token.Error() != nil {
				slog.Error("failed to subscribe", "topic", topic, "error", token.Error())
			}
		})

	c.mqttClient = mqtt.NewClient(opts)
	token := c.mqttClient.Connect()
	if !token.WaitTimeout(30 * time.Second) {
		return fmt.Errorf("MQTT connect timeout")
	}
	if token.Error() != nil {
		return fmt.Errorf("MQTT connect: %w", token.Error())
	}

	// Request full status to populate initial state
	c.RequestStatus()
	return nil
}

// Disconnect cleanly shuts down the MQTT connection.
func (c *Client) Disconnect() {
	if c.mqttClient != nil && c.mqttClient.IsConnected() {
		c.mqttClient.Disconnect(1000)
	}
}

// State returns a snapshot of the current merged printer state.
func (c *Client) State() MergedState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// UpdateCh returns a channel that receives a signal whenever new data arrives.
func (c *Client) UpdateCh() <-chan struct{} {
	return c.updateCh
}

// RequestStatus sends a push_status request to get a full state refresh.
// Useful when joining mid-print (especially for P1/A1 delta-only printers).
func (c *Client) RequestStatus() {
	topic := fmt.Sprintf("device/%s/request", c.serial)
	payload := `{"pushing":{"command":"pushall","sequence_id":"0"}}`
	if token := c.mqttClient.Publish(topic, 0, false, payload); token.Wait() && token.Error() != nil {
		slog.Warn("failed to request status", "error", token.Error())
	}
}

func (c *Client) onMessage(_ mqtt.Client, msg mqtt.Message) {
	var report Report
	if err := json.Unmarshal(msg.Payload(), &report); err != nil {
		slog.Debug("ignoring non-JSON MQTT message", "error", err)
		return
	}

	if report.Print == nil || report.Print.Command != "push_status" {
		return
	}

	c.mu.Lock()
	c.state.Merge(report.Print)
	c.mu.Unlock()

	// Non-blocking signal to the tracker that new data is available
	select {
	case c.updateCh <- struct{}{}:
	default:
	}
}
