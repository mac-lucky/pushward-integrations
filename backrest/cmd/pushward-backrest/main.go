package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	brclient "github.com/mac-lucky/pushward-integrations/backrest/internal/backrest"
	"github.com/mac-lucky/pushward-integrations/backrest/internal/config"
	"github.com/mac-lucky/pushward-integrations/backrest/internal/poller"
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

	br := brclient.NewClient(cfg.Backrest.URL, brclient.Options{
		Username: cfg.Backrest.Username,
		Password: cfg.Backrest.Password,
		Token:    cfg.Backrest.Token,
		Timeout:  cfg.Backrest.Timeout,
	})
	pw := pushward.NewClient(cfg.PushWard.URL, cfg.PushWard.APIKey)

	p := poller.New(cfg, br, pw)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.Info("starting pushward-backrest",
		"backrest_url", cfg.Backrest.URL,
		"interval", cfg.Polling.Interval,
		"idle_interval", cfg.Polling.IdleInterval,
		"live_progress", cfg.Render.LiveProgress,
		"logs", cfg.Render.Logs,
		"priority", cfg.PushWard.Priority)

	if err := p.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("poller exited with error", "error", err)
		os.Exit(1)
	}
	slog.Info("shutdown complete")
}
