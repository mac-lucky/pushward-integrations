package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mac-lucky/pushward-integrations/bambulab/internal/bambulab"
	"github.com/mac-lucky/pushward-integrations/bambulab/internal/config"
	"github.com/mac-lucky/pushward-integrations/bambulab/internal/tracker"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// initialConnectRetry is how long to wait between attempts to establish the
// first MQTT connection when the printer is unreachable at startup.
const initialConnectRetry = 30 * time.Second

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

	bambu, err := bambulab.NewClient(
		cfg.BambuLab.Host,
		cfg.BambuLab.AccessCode,
		cfg.BambuLab.Serial,
		cfg.BambuLab.TLS.InsecureSkipVerify,
		cfg.BambuLab.TLS.CertFingerprintSHA256,
	)
	if err != nil {
		slog.Error("failed to build bambulab client", "error", err)
		os.Exit(1)
	}
	pw := pushward.NewClient(cfg.PushWard.URL, cfg.PushWard.APIKey)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// 3D printers are frequently powered off when idle, and paho's
	// auto-reconnect only engages after the first successful connection — so a
	// hard exit here would crashloop the pod (with growing k8s backoff) and miss
	// the print-start event. Retry the initial connect until the printer appears
	// or we're asked to shut down.
	slog.Info("connecting to BambuLab printer", "host", cfg.BambuLab.Host, "serial", cfg.BambuLab.Serial)
	for {
		if err := bambu.Connect(); err != nil {
			slog.Warn("failed to connect to printer, retrying", "error", err, "retry_in", initialConnectRetry)
			select {
			case <-ctx.Done():
				slog.Info("shutdown requested before initial connection established")
				return
			case <-time.After(initialConnectRetry):
				continue
			}
		}
		break
	}
	defer bambu.Disconnect()

	t := tracker.New(cfg, bambu, pw)

	slog.Info("starting pushward-bambulab", "priority", cfg.PushWard.Priority, "update_interval", cfg.Polling.UpdateInterval)
	if err := t.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("tracker exited with error", "error", err)
		os.Exit(1)
	}
	slog.Info("shutdown complete")
}
