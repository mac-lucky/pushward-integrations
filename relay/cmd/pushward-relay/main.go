package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/argocd"
	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/backrest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/changedetection"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/gatus"
	"github.com/mac-lucky/pushward-integrations/relay/internal/grafana"
	"github.com/mac-lucky/pushward-integrations/relay/internal/jellyfin"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overseerr"
	"github.com/mac-lucky/pushward-integrations/relay/internal/paperless"
	"github.com/mac-lucky/pushward-integrations/relay/internal/proxmox"
	"github.com/mac-lucky/pushward-integrations/relay/internal/ratelimit"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/starr"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/unmanic"
	"github.com/mac-lucky/pushward-integrations/relay/internal/uptimekuma"
	"github.com/mac-lucky/pushward-integrations/shared/server"
)

func main() {
	configPath := flag.String("config", "config.yml", "path to config file")
	pushwardURL := flag.String("pushward-url", "", "PushWard server URL (overrides config)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	baseURL := *pushwardURL
	if baseURL == "" {
		baseURL = os.Getenv("PUSHWARD_URL")
	}
	if baseURL == "" {
		slog.Error("pushward URL is required (set PUSHWARD_URL or use -pushward-url)")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Database
	pool, err := state.NewPool(ctx, cfg.Database.DSN, cfg.Database.PasswordFile)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	slog.Info("connected to database")

	// State store
	store, err := state.NewPostgresStore(ctx, pool)
	if err != nil {
		slog.Error("failed to initialize state store", "error", err)
		os.Exit(1)
	}

	// Client pool
	clients := client.NewPool(baseURL)

	// Router
	mux := server.NewMux()

	// Test endpoint — always enabled so users can verify the relay works
	pth := selftest.NewProviderTestHandler(clients)
	mux.Handle("POST /test/{provider}", ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(pth))))

	// Provider handlers — each route is wrapped with IP rate limit → auth → key rate limit
	if cfg.Providers.Grafana.Enabled {
		gh := grafana.NewHandler(store, clients, &cfg.Providers.Grafana)
		mux.Handle("POST /grafana", ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(gh))))
		slog.Info("enabled provider", "provider", "grafana")
	}

	if cfg.Providers.ArgoCD.Enabled {
		ah := argocd.NewHandler(store, clients, &cfg.Providers.ArgoCD)
		mux.Handle("POST /argocd", ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(ah))))
		slog.Info("enabled provider", "provider", "argocd")
	}

	if cfg.Providers.Starr.Enabled {
		sh := starr.NewHandler(store, clients, &cfg.Providers.Starr)
		mux.Handle("POST /radarr", ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(sh.RadarrHandler()))))
		mux.Handle("POST /sonarr", ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(sh.SonarrHandler()))))
		slog.Info("enabled provider", "provider", "starr")
	}

	if cfg.Providers.Jellyfin.Enabled {
		jh := jellyfin.NewHandler(store, clients, &cfg.Providers.Jellyfin)
		mux.Handle("POST /jellyfin", ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(jh))))
		slog.Info("enabled provider", "provider", "jellyfin")
	}

	if cfg.Providers.Paperless.Enabled {
		ph := paperless.NewHandler(store, clients, &cfg.Providers.Paperless)
		mux.Handle("POST /paperless", ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(ph))))
		slog.Info("enabled provider", "provider", "paperless")
	}

	if cfg.Providers.Changedetection.Enabled {
		cdh := changedetection.NewHandler(clients, &cfg.Providers.Changedetection)
		mux.Handle("POST /changedetection", ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(cdh))))
		slog.Info("enabled provider", "provider", "changedetection")
	}

	if cfg.Providers.Unmanic.Enabled {
		uh := unmanic.NewHandler(clients, &cfg.Providers.Unmanic)
		mux.Handle("POST /unmanic", ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(uh))))
		slog.Info("enabled provider", "provider", "unmanic")
	}

	if cfg.Providers.Proxmox.Enabled {
		pxh := proxmox.NewHandler(store, clients, &cfg.Providers.Proxmox)
		mux.Handle("POST /proxmox", ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(pxh))))
		slog.Info("enabled provider", "provider", "proxmox")
	}

	if cfg.Providers.Overseerr.Enabled {
		oh := overseerr.NewHandler(store, clients, &cfg.Providers.Overseerr)
		mux.Handle("POST /overseerr", ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(oh))))
		slog.Info("enabled provider", "provider", "overseerr")
	}

	if cfg.Providers.UptimeKuma.Enabled {
		ukh := uptimekuma.NewHandler(store, clients, &cfg.Providers.UptimeKuma)
		mux.Handle("POST /uptimekuma", ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(ukh))))
		slog.Info("enabled provider", "provider", "uptimekuma")
	}

	if cfg.Providers.Gatus.Enabled {
		gah := gatus.NewHandler(store, clients, &cfg.Providers.Gatus)
		mux.Handle("POST /gatus", ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(gah))))
		slog.Info("enabled provider", "provider", "gatus")
	}

	if cfg.Providers.Backrest.Enabled {
		bh := backrest.NewHandler(store, clients, &cfg.Providers.Backrest)
		mux.Handle("POST /backrest", ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(bh))))
		slog.Info("enabled provider", "provider", "backrest")
	}

	// Background cleanup goroutine
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := store.Cleanup(ctx); err != nil {
					slog.Error("state cleanup failed", "error", err)
				} else if n > 0 {
					slog.Info("state cleanup", "removed", n)
				}
			}
		}
	}()

	slog.Info("starting pushward-relay", "address", cfg.Server.Address)
	if err := server.ListenAndServe(ctx, cfg.Server.Address, mux); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
	slog.Info("shutdown complete")
}
