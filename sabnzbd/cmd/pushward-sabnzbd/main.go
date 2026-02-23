package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mac-lucky/pushward-docker/sabnzbd/internal/config"
	"github.com/mac-lucky/pushward-docker/sabnzbd/internal/sabnzbd"
	"github.com/mac-lucky/pushward-docker/sabnzbd/internal/tracker"
	"github.com/mac-lucky/pushward-docker/shared/pushward"
	"github.com/mac-lucky/pushward-docker/shared/server"
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

	sab := sabnzbd.NewClient(cfg.SABnzbd.URL, cfg.SABnzbd.APIKey)
	pw := pushward.NewClient(cfg.PushWard.URL, cfg.PushWard.APIKey)
	t := tracker.New(cfg, sab, pw)

	mux := server.NewMux()
	mux.HandleFunc("/webhook", t.HandleWebhook)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if !t.ResumeIfActive() {
		t.Cleanup(ctx)
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
