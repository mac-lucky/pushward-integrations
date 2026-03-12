package mqtt

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/mac-lucky/pushward-integrations/mqtt/internal/config"
)

// Client wraps the paho MQTT client with configurable TLS and multi-topic subscription.
type Client struct {
	mqttClient paho.Client
	config     *config.MQTTConfig
	topics     []string
	handler    func(topic string, payload []byte)
}

// NewClient creates a new MQTT client. handler is called for every incoming message.
func NewClient(cfg *config.MQTTConfig, topics []string, handler func(topic string, payload []byte)) *Client {
	return &Client{
		config:  cfg,
		topics:  topics,
		handler: handler,
	}
}

// Connect establishes the MQTT connection and subscribes to all topics.
func (c *Client) Connect() error {
	opts := paho.NewClientOptions().
		AddBroker(c.config.Broker).
		SetClientID(c.config.ClientID).
		SetKeepAlive(60 * time.Second).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30 * time.Second).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			slog.Warn("MQTT connection lost", "error", err)
		}).
		SetOnConnectHandler(func(client paho.Client) {
			slog.Info("MQTT connected, subscribing to topics", "count", len(c.topics))
			for _, topic := range c.topics {
				if token := client.Subscribe(topic, 0, c.onMessage); token.Wait() && token.Error() != nil {
					slog.Error("failed to subscribe", "topic", topic, "error", token.Error())
				} else {
					slog.Info("subscribed", "topic", topic)
				}
			}
		})

	if c.config.Username != "" {
		opts.SetUsername(c.config.Username)
	}
	if c.config.Password != "" {
		opts.SetPassword(c.config.Password)
	}

	if c.config.TLS.Enabled {
		tlsCfg, err := buildTLSConfig(&c.config.TLS)
		if err != nil {
			return fmt.Errorf("building TLS config: %w", err)
		}
		opts.SetTLSConfig(tlsCfg)
	}

	c.mqttClient = paho.NewClient(opts)
	token := c.mqttClient.Connect()
	if !token.WaitTimeout(30 * time.Second) {
		return fmt.Errorf("MQTT connect timeout")
	}
	if token.Error() != nil {
		return fmt.Errorf("MQTT connect: %w", token.Error())
	}

	return nil
}

// Disconnect cleanly shuts down the MQTT connection.
func (c *Client) Disconnect() {
	if c.mqttClient != nil && c.mqttClient.IsConnected() {
		c.mqttClient.Disconnect(1000)
	}
}

func (c *Client) onMessage(_ paho.Client, msg paho.Message) {
	c.handler(msg.Topic(), msg.Payload())
}

func buildTLSConfig(cfg *config.TLSConfig) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}

	if cfg.CACert != "" {
		caCert, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("reading CA cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA cert")
		}
		tlsCfg.RootCAs = pool
	}

	if cfg.ClientCert != "" && cfg.ClientKey != "" {
		cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("loading client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	return tlsCfg, nil
}
