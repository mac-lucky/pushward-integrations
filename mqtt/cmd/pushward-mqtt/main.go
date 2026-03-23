package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mac-lucky/pushward-integrations/mqtt/internal/config"
	"github.com/mac-lucky/pushward-integrations/mqtt/internal/engine"
	mqttclient "github.com/mac-lucky/pushward-integrations/mqtt/internal/mqtt"
	"github.com/mac-lucky/pushward-integrations/mqtt/internal/tracker"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

func main() {
	configPath := flag.String("config", "config.yml", "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	pw := pushward.NewClient(cfg.PushWard.URL, cfg.PushWard.APIKey)

	trackerCfg := &tracker.Config{
		EndDelay:       cfg.PushWard.EndDelay,
		EndDisplayTime: cfg.PushWard.EndDisplayTime,
		CleanupDelay:   cfg.PushWard.CleanupDelay,
		StaleTimeout:   cfg.PushWard.StaleTimeout,
		UpdateInterval: cfg.Polling.UpdateInterval,
	}

	var entries []*engine.RuleEntry
	for i := range cfg.Rules {
		rule := &cfg.Rules[i]
		tr := tracker.New(rule, pw, cfg.PushWard.Priority, trackerCfg)
		entries = append(entries, engine.NewRuleEntry(rule, tr))
	}

	eng := engine.New(entries)
	topics := eng.Topics()

	client := mqttclient.NewClient(&cfg.MQTT, topics, eng.Route)

	slog.Info("connecting to MQTT broker", "broker", cfg.MQTT.Broker, "topics", len(topics))
	if err := client.Connect(); err != nil {
		slog.Error("failed to connect to MQTT broker", "error", err)
		os.Exit(1)
	}
	defer client.Disconnect()

	slog.Info("pushward-mqtt started", "rules", len(cfg.Rules), "priority", cfg.PushWard.Priority, "update_interval", cfg.Polling.UpdateInterval)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	<-ctx.Done()

	slog.Info("shutting down")
	eng.Stop()
	slog.Info("shutdown complete")
}
