package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mac-lucky/pushward-integrations/unraid/internal/config"
	"github.com/mac-lucky/pushward-integrations/unraid/internal/graphql"
	"github.com/mac-lucky/pushward-integrations/unraid/internal/tracker"

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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	gql := graphql.NewClient(cfg.Unraid.Host, cfg.Unraid.Port, cfg.Unraid.APIKey, cfg.Unraid.UseTLS)
	pw := pushward.NewClient(cfg.PushWard.URL, cfg.PushWard.APIKey)
	t := tracker.New(cfg, gql, pw)

	slog.Info("starting pushward-unraid", "host", cfg.Unraid.Host, "port", cfg.Unraid.Port)
	if err := t.Run(ctx); err != nil {
		slog.Error("tracker error", "error", err)
		os.Exit(1)
	}
	slog.Info("shutdown complete")
}
