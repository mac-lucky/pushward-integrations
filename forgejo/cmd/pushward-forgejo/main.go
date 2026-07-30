package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mac-lucky/pushward-integrations/forgejo/internal/config"
	fjclient "github.com/mac-lucky/pushward-integrations/forgejo/internal/forgejo"
	"github.com/mac-lucky/pushward-integrations/forgejo/internal/poller"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
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

	fj := fjclient.NewClient(cfg.Forgejo.URL, cfg.Forgejo.Token, fjclient.Options{
		Timeout: cfg.Forgejo.Timeout,
		// The timing join costs an extra tasks lookup per poll, so only ask for it
		// when something actually renders the result.
		LiveTimings:    cfg.Render.LiveProgress,
		HistoryTimings: cfg.Render.StepWeights || cfg.Render.LiveProgress,
	})
	pw := pushward.NewClient(cfg.PushWard.URL, cfg.PushWard.APIKey)

	p := poller.New(cfg, fj, pw)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Probe the instance once so a support question about odd behavior has the
	// release to hand. Best-effort: a homelab Forgejo that is briefly unreachable
	// at startup should back off and retry, not crashloop.
	logVersion(ctx, fj)

	slog.Info("starting pushward-forgejo",
		"url", text.SanitizeURL(cfg.Forgejo.URL),
		"owner", cfg.Forgejo.Owner,
		"repos", cfg.Forgejo.Repos,
		"poll_idle", cfg.Polling.IdleInterval,
		"priority", cfg.PushWard.Priority,
		"cleanup_delay", cfg.PushWard.CleanupDelay)

	if err := p.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("poller exited with error", "error", err)
		os.Exit(1)
	}
	slog.Info("shutdown complete")
}

func logVersion(ctx context.Context, fj *fjclient.Client) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	version, err := fj.GetVersion(probeCtx)
	if err != nil {
		slog.Warn("could not read the Forgejo version", "error", err)
		return
	}
	slog.Info("connected to Forgejo", "version", version)
}
