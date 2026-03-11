package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mac-lucky/pushward-integrations/grafana/internal/config"
	"github.com/mac-lucky/pushward-integrations/grafana/internal/handler"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/server"
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
	h := handler.New(pw, cfg)

	mux := server.NewMux()
	mux.HandleFunc("/webhook", h.HandleWebhook)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.Info("starting pushward-grafana", "address", cfg.Server.Address, "priority", cfg.PushWard.Priority, "cleanup_delay", cfg.PushWard.CleanupDelay, "stale_timeout", cfg.PushWard.StaleTimeout)
	if err := server.ListenAndServe(ctx, cfg.Server.Address, mux); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
	slog.Info("shutdown complete")
}
