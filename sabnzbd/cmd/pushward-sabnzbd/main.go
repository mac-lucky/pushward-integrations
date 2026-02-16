package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/mac-lucky/pushward-docker/sabnzbd/internal/config"
	"github.com/mac-lucky/pushward-docker/sabnzbd/internal/pushward"
	"github.com/mac-lucky/pushward-docker/sabnzbd/internal/sabnzbd"
	"github.com/mac-lucky/pushward-docker/sabnzbd/internal/tracker"
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

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", t.HandleWebhook)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    cfg.Server.Address,
		Handler: mux,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	t.Cleanup(ctx)

	go func() {
		slog.Info("starting pushward-sabnzbd", "address", cfg.Server.Address, "priority", cfg.PushWard.Priority, "cleanup_delay", cfg.PushWard.CleanupDelay)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down server")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)

	slog.Info("waiting for active tracking to finish")
	t.Wait()
	slog.Info("shutdown complete")
}
