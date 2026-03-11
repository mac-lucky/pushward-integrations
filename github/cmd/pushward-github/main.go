package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mac-lucky/pushward-integrations/github/internal/config"
	ghclient "github.com/mac-lucky/pushward-integrations/github/internal/github"
	"github.com/mac-lucky/pushward-integrations/github/internal/poller"
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

	gh := ghclient.NewClient(cfg.GitHub.Token)
	pw := pushward.NewClient(cfg.PushWard.URL, cfg.PushWard.APIKey)

	p := poller.New(cfg, gh, pw)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.Info("starting pushward-github", "owner", cfg.GitHub.Owner, "repos", cfg.GitHub.Repos, "priority", cfg.PushWard.Priority, "cleanup_delay", cfg.PushWard.CleanupDelay)
	if err := p.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("poller exited with error", "error", err)
		os.Exit(1)
	}
	slog.Info("shutdown complete")
}
