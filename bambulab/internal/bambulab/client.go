package bambulab

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	certFingerprint    []byte // SHA-256, parsed from hex; len(0) means no pinning

	mqttClient mqtt.Client
	mu         sync.Mutex
	state      MergedState
	updateCh   chan struct{} // signals new data available
}

// NewClient creates a new BambuLab MQTT client. When certFingerprintSHA256 is
// non-empty, TLS verification uses fingerprint pinning against the printer's
// self-signed cert instead of skipping verification.
func NewClient(host, accessCode, serial string, insecureSkipVerify bool, certFingerprintSHA256 string) (*Client, error) {
	c := &Client{
		host:               host,
		accessCode:         accessCode,
		serial:             serial,
		insecureSkipVerify: insecureSkipVerify,
		updateCh:           make(chan struct{}, 1),
	}

	if fp := strings.TrimSpace(certFingerprintSHA256); fp != "" {
		raw, err := hex.DecodeString(strings.ReplaceAll(fp, ":", ""))
		if err != nil {
			return nil, fmt.Errorf("parsing cert_fingerprint_sha256: %w", err)
		}
		if len(raw) != sha256.Size {
			return nil, fmt.Errorf("cert_fingerprint_sha256 must be %d bytes (SHA-256), got %d", sha256.Size, len(raw))
		}
		c.certFingerprint = raw
	}

	return c, nil
}

// Connect establishes the MQTT connection and subscribes to the report topic.
func (c *Client) Connect() error {
	broker := fmt.Sprintf("tls://%s:8883", c.host)
	topic := fmt.Sprintf("device/%s/report", c.serial)

	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(fmt.Sprintf("pushward-bambulab-%s", c.serial)).
		SetCleanSession(false).
		SetUsername("bblp").
		SetPassword(c.accessCode).
		SetKeepAlive(60 * time.Second).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30 * time.Second).
		SetTLSConfig(c.tlsConfig()).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			slog.Warn("MQTT connection lost", "error", err)
		}).
		SetOnConnectHandler(func(client mqtt.Client) {
			slog.Info("MQTT connected, subscribing", "topic", topic)
			if token := client.Subscribe(topic, 1, c.onMessage); token.Wait() && token.Error() != nil {
				slog.Error("failed to subscribe, disconnecting", "topic", topic, "error", token.Error())
				go client.Disconnect(0)
				return
			}
			// Re-request full state on every connect (including reconnects) so
			// delta-only printers (P1/A1) don't hold stale MergedState.
			c.RequestStatus()
		})

	c.mqttClient = mqtt.NewClient(opts)
	token := c.mqttClient.Connect()
	if !token.WaitTimeout(30 * time.Second) {
		return fmt.Errorf("MQTT connect timeout")
	}
	if token.Error() != nil {
		return fmt.Errorf("MQTT connect: %w", token.Error())
	}

	return nil
}

func (c *Client) tlsConfig() *tls.Config {
	if len(c.certFingerprint) == sha256.Size {
		return &tls.Config{
			InsecureSkipVerify: true, // #nosec G402 -- pinned via VerifyConnection
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("bambulab: no peer certificate")
				}
				got := sha256.Sum256(cs.PeerCertificates[0].Raw)
				if subtle.ConstantTimeCompare(got[:], c.certFingerprint) != 1 {
					return errors.New("bambulab: peer cert fingerprint mismatch")
				}
				return nil
			},
		}
	}
	if c.insecureSkipVerify {
		slog.Warn("BambuLab TLS verification disabled; set bambulab.tls.cert_fingerprint_sha256 to pin the printer cert")
	}
	return &tls.Config{InsecureSkipVerify: c.insecureSkipVerify} // #nosec G402
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
	if c.mqttClient == nil {
		return
	}
	topic := fmt.Sprintf("device/%s/request", c.serial)
	payload := `{"pushing":{"command":"pushall","sequence_id":"0"}}`
	if token := c.mqttClient.Publish(topic, 1, false, payload); token.Wait() && token.Error() != nil {
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
