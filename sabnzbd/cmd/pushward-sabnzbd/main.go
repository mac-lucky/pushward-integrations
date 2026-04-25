package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mac-lucky/pushward-integrations/sabnzbd/internal/config"
	"github.com/mac-lucky/pushward-integrations/sabnzbd/internal/sabnzbd"
	"github.com/mac-lucky/pushward-integrations/sabnzbd/internal/tracker"
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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	sab := sabnzbd.NewClient(cfg.SABnzbd.URL, cfg.SABnzbd.APIKey)
	pw := pushward.NewClient(cfg.PushWard.URL, cfg.PushWard.APIKey,
		pushward.WithCircuitBreaker(pushward.NewCircuitBreaker(5, 30*time.Second)))
	t := tracker.New(cfg, sab, pw)

	mux := server.NewMux()
	mux.HandleFunc("/webhook", t.WebhookHandler(ctx))

	if !t.ResumeIfActive(ctx) {
		t.Cleanup(ctx)
	}

	if cfg.SABnzbd.WebhookSecret == "" {
		slog.Warn("webhook secret not configured — webhook endpoint is unauthenticated",
			"hint", "set sabnzbd.webhook_secret or PUSHWARD_SABNZBD_WEBHOOK_SECRET")
	}

	slog.Info("starting pushward-sabnzbd", "address", cfg.Server.Address, "priority", cfg.PushWard.Priority, "cleanup_delay", cfg.PushWard.CleanupDelay)
	if err := server.ListenAndServe(ctx, cfg.Server.Address, mux); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}

	slog.Info("waiting for active tracking to finish")
	t.Wait()
	slog.Info("shutdown complete")
}
