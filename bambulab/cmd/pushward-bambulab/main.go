package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mac-lucky/pushward-docker/bambulab/internal/bambulab"
	"github.com/mac-lucky/pushward-docker/bambulab/internal/config"
	"github.com/mac-lucky/pushward-docker/bambulab/internal/tracker"
	"github.com/mac-lucky/pushward-docker/shared/pushward"
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

	bambu := bambulab.NewClient(cfg.BambuLab.Host, cfg.BambuLab.AccessCode, cfg.BambuLab.Serial)
	pw := pushward.NewClient(cfg.PushWard.URL, cfg.PushWard.APIKey)

	slog.Info("connecting to BambuLab printer", "host", cfg.BambuLab.Host, "serial", cfg.BambuLab.Serial)
	if err := bambu.Connect(); err != nil {
		slog.Error("failed to connect to printer", "error", err)
		os.Exit(1)
	}
	defer bambu.Disconnect()

	t := tracker.New(cfg, bambu, pw)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.Info("starting pushward-bambulab", "priority", cfg.PushWard.Priority, "update_interval", cfg.Polling.UpdateInterval)
	if err := t.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("tracker exited with error", "error", err)
		os.Exit(1)
	}
	slog.Info("shutdown complete")
}
