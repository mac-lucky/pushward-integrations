package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/mac-lucky/pushward-integrations/grafana/internal/config"
	grafanaapi "github.com/mac-lucky/pushward-integrations/grafana/internal/grafana"
	"github.com/mac-lucky/pushward-integrations/grafana/internal/handler"
	"github.com/mac-lucky/pushward-integrations/grafana/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/grafana/internal/poller"
	sharedauth "github.com/mac-lucky/pushward-integrations/shared/auth"
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

	pwClient := pushward.NewClient(cfg.PushWard.URL, cfg.PushWard.APIKey)

	var metricsOpts []metrics.Option
	if cfg.Metrics.Username != "" {
		metricsOpts = append(metricsOpts, metrics.WithBasicAuth(cfg.Metrics.Username, cfg.Metrics.Password))
	}
	if cfg.Metrics.BearerToken != "" {
		metricsOpts = append(metricsOpts, metrics.WithBearerToken(cfg.Metrics.BearerToken))
	}
	mc := metrics.NewClient(cfg.Metrics.URL, metricsOpts...)

	var gc *grafanaapi.Client
	if cfg.AutoExtractEnabled() {
		gc = grafanaapi.NewClient(cfg.Grafana.URL, cfg.Grafana.APIToken)
		slog.Info("grafana auto-extract enabled", "url", cfg.Grafana.URL)
	}

	p := poller.New(mc, pwClient, cfg.Timeline.PollInterval)

	h := handler.NewHandler(pwClient, mc, gc, p, handler.Config{
		HistoryWindow:   cfg.Timeline.HistoryWindow,
		Priority:        cfg.PushWard.Priority,
		CleanupDelay:    cfg.PushWard.CleanupDelay,
		StaleTimeout:    cfg.PushWard.StaleTimeout,
		SeverityLabel:   cfg.Timeline.SeverityLabel,
		DefaultSeverity: cfg.Timeline.DefaultSeverity,
		Smoothing:       cfg.Timeline.Smoothing,
		Scale:           cfg.Timeline.Scale,
		Decimals:        cfg.Timeline.Decimals,
	})

	h.StartSweeper(ctx, cfg.PushWard.StaleTimeout)
	h.StartAlertChecker(ctx, cfg.Grafana.AlertCheckInterval)

	mux := server.NewMux()
	var webhookHandler http.Handler = h
	if cfg.WebhookToken != "" {
		webhookHandler = sharedauth.RequireHeader("Authorization", "Bearer "+cfg.WebhookToken)(h)
		slog.Info("webhook bearer auth enabled")
	} else {
		slog.Warn("webhook token not configured — webhook endpoint is unauthenticated",
			"hint", "set webhook_token or PUSHWARD_WEBHOOK_TOKEN")
	}
	mux.Handle("POST /webhook", webhookHandler)

	slog.Info("starting pushward-grafana",
		"address", cfg.Server.Address,
		"metrics_url", cfg.Metrics.URL,
		"auto_extract", cfg.AutoExtractEnabled(),
		"history_window", cfg.Timeline.HistoryWindow,
		"poll_interval", cfg.Timeline.PollInterval,
	)

	if err := server.ListenAndServe(ctx, cfg.Server.Address, mux); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}

	slog.Info("shutting down")
	h.WaitIdle()
	p.StopAll()
	p.Wait()
	slog.Info("shutdown complete")
}
