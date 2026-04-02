package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/mac-lucky/pushward-integrations/relay/internal/argocd"
	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/backrest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/changedetection"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/gatus"
	"github.com/mac-lucky/pushward-integrations/relay/internal/grafana"
	"github.com/mac-lucky/pushward-integrations/relay/internal/jellyfin"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overseerr"
	"github.com/mac-lucky/pushward-integrations/relay/internal/paperless"
	"github.com/mac-lucky/pushward-integrations/relay/internal/proxmox"
	"github.com/mac-lucky/pushward-integrations/relay/internal/ratelimit"
	"github.com/mac-lucky/pushward-integrations/relay/internal/starr"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/telemetry"
	"github.com/mac-lucky/pushward-integrations/relay/internal/unmanic"
	"github.com/mac-lucky/pushward-integrations/relay/internal/uptimekuma"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
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

	// Initialize OpenTelemetry tracing (noop when endpoint is empty).
	otelShutdown, err := telemetry.Init(context.Background(), telemetry.Config{
		Endpoint:    cfg.Telemetry.Endpoint,
		TLSCertPath: cfg.Telemetry.TLSCertPath,
		TLSKeyPath:  cfg.Telemetry.TLSKeyPath,
		ServiceName: "pushward-relay",
		Environment: "production",
		SampleRate:  cfg.Telemetry.SampleRate,
	})
	if err != nil {
		slog.Error("failed to initialize telemetry", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := otelShutdown(shutdownCtx); err != nil {
			slog.Error("telemetry shutdown error", "error", err)
		}
	}()

	// Configure trusted proxy CIDRs for IP rate limiting
	if len(cfg.TrustedProxyCIDRs) > 0 {
		if err := ratelimit.SetTrustedProxyCIDRs(cfg.TrustedProxyCIDRs); err != nil {
			slog.Error("failed to parse trusted proxy CIDRs", "error", err)
			os.Exit(1)
		}
		slog.Info("configured trusted proxy CIDRs", "count", len(cfg.TrustedProxyCIDRs))
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

	// Client pool — use an instrumented transport when tracing is enabled
	// so outbound requests propagate trace context to pushward-server.
	var httpClient *http.Client
	if cfg.Telemetry.Endpoint != "" {
		httpClient = &http.Client{
			Timeout:   10 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		}
	}
	breaker := pushward.NewCircuitBreaker(cfg.CircuitBreaker.Threshold, cfg.CircuitBreaker.Cooldown)
	clients := client.NewPool(baseURL, httpClient,
		pushward.WithOnResult(metrics.RecordAPICall),
		pushward.WithCircuitBreaker(breaker),
	)
	slog.Info("circuit breaker configured", "threshold", cfg.CircuitBreaker.Threshold, "cooldown", cfg.CircuitBreaker.Cooldown)

	// Router
	mux := server.NewMux(pool.Ping)
	mux.Handle("GET /metrics", metrics.Handler())

	// wrapHandler applies the standard middleware chain: IP rate limit → auth → key rate limit.
	wrapHandler := func(h http.Handler) http.Handler {
		return ratelimit.IPMiddleware(auth.Middleware(ratelimit.Middleware(h)))
	}

	// Provider handlers
	var enders []*lifecycle.Ender

	// collectEnder appends the handler's Ender if it implements lifecycle.EnderProvider.
	collectEnder := func(handler any) {
		if ep, ok := handler.(lifecycle.EnderProvider); ok {
			enders = append(enders, ep.Ender())
		}
	}

	if cfg.Providers.Grafana.Enabled {
		gh := grafana.NewHandler(store, clients, &cfg.Providers.Grafana)
		mux.Handle("POST /grafana", wrapHandler(gh))
		slog.Info("enabled provider", "provider", "grafana")
	}

	if cfg.Providers.ArgoCD.Enabled {
		ah := argocd.NewHandler(store, clients, &cfg.Providers.ArgoCD)
		mux.Handle("POST /argocd", wrapHandler(ah))
		collectEnder(ah)
		ah.StartCleanup(ctx)
		slog.Info("enabled provider", "provider", "argocd")
	}

	if cfg.Providers.Starr.Enabled {
		sh := starr.NewHandler(store, clients, &cfg.Providers.Starr)
		mux.Handle("POST /radarr", wrapHandler(sh.RadarrHandler()))
		mux.Handle("POST /sonarr", wrapHandler(sh.SonarrHandler()))
		collectEnder(sh)
		slog.Info("enabled provider", "provider", "starr")
	}

	if cfg.Providers.Jellyfin.Enabled {
		jh := jellyfin.NewHandler(store, clients, &cfg.Providers.Jellyfin)
		mux.Handle("POST /jellyfin", wrapHandler(jh))
		collectEnder(jh)
		jh.StartCleanup(ctx)
		slog.Info("enabled provider", "provider", "jellyfin")
	}

	if cfg.Providers.Paperless.Enabled {
		ph := paperless.NewHandler(store, clients, &cfg.Providers.Paperless)
		mux.Handle("POST /paperless", wrapHandler(ph))
		collectEnder(ph)
		slog.Info("enabled provider", "provider", "paperless")
	}

	if cfg.Providers.Changedetection.Enabled {
		cdh := changedetection.NewHandler(clients, &cfg.Providers.Changedetection)
		mux.Handle("POST /changedetection", wrapHandler(cdh))
		slog.Info("enabled provider", "provider", "changedetection")
	}

	if cfg.Providers.Unmanic.Enabled {
		uh := unmanic.NewHandler(clients, &cfg.Providers.Unmanic)
		mux.Handle("POST /unmanic", wrapHandler(uh))
		collectEnder(uh)
		slog.Info("enabled provider", "provider", "unmanic")
	}

	if cfg.Providers.Proxmox.Enabled {
		pxh := proxmox.NewHandler(store, clients, &cfg.Providers.Proxmox)
		mux.Handle("POST /proxmox", wrapHandler(pxh))
		collectEnder(pxh)
		slog.Info("enabled provider", "provider", "proxmox")
	}

	if cfg.Providers.Overseerr.Enabled {
		oh := overseerr.NewHandler(store, clients, &cfg.Providers.Overseerr)
		mux.Handle("POST /overseerr", wrapHandler(oh))
		collectEnder(oh)
		slog.Info("enabled provider", "provider", "overseerr")
	}

	if cfg.Providers.UptimeKuma.Enabled {
		ukh := uptimekuma.NewHandler(store, clients, &cfg.Providers.UptimeKuma)
		mux.Handle("POST /uptimekuma", wrapHandler(ukh))
		collectEnder(ukh)
		slog.Info("enabled provider", "provider", "uptimekuma")
	}

	if cfg.Providers.Gatus.Enabled {
		gah := gatus.NewHandler(store, clients, &cfg.Providers.Gatus)
		mux.Handle("POST /gatus", wrapHandler(gah))
		collectEnder(gah)
		slog.Info("enabled provider", "provider", "gatus")
	}

	if cfg.Providers.Backrest.Enabled {
		bh := backrest.NewHandler(store, clients, &cfg.Providers.Backrest)
		mux.Handle("POST /backrest", wrapHandler(bh))
		collectEnder(bh)
		slog.Info("enabled provider", "provider", "backrest")
	}

	// Wrap mux with metrics middleware and optional OTel tracing.
	var handler http.Handler = metrics.Middleware(mux)
	if cfg.Telemetry.Endpoint != "" {
		handler = otelhttp.NewHandler(handler, "pushward-relay",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				if r.Pattern != "" {
					return r.Pattern
				}
				return r.Method
			}),
			otelhttp.WithFilter(func(r *http.Request) bool {
				return r.URL.Path != "/health" && r.URL.Path != "/metrics" && r.URL.Path != "/ready"
			}),
		)
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

	// Background goroutine: collect DB + circuit breaker metrics every 15s
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				stat := pool.Stat()
				metrics.DBPoolTotalConns.Set(float64(stat.TotalConns()))
				metrics.DBPoolIdleConns.Set(float64(stat.IdleConns()))
				metrics.DBPoolAcquiredConns.Set(float64(stat.AcquiredConns()))
				val := 0.0
				if breaker.IsOpen() {
					val = 1
				}
				metrics.CircuitBreakerOpen.Set(val)
			}
		}
	}()

	slog.Info("starting pushward-relay", "address", cfg.Server.Address)
	if err := server.ListenAndServe(ctx, cfg.Server.Address, handler); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}

	// Stop all pending ender timers, then wait for in-flight callbacks.
	for _, e := range enders {
		e.StopAll()
	}
	for _, e := range enders {
		e.Wait()
	}

	slog.Info("shutdown complete")
}
