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

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/mac-lucky/pushward-integrations/relay/internal/argocd"
	"github.com/mac-lucky/pushward-integrations/relay/internal/backrest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/bazarr"
	"github.com/mac-lucky/pushward-integrations/relay/internal/changedetection"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/gatus"
	"github.com/mac-lucky/pushward-integrations/relay/internal/grafana"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
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

	// Huma API — auto-generates OpenAPI 3.1 spec at /openapi.json and docs at /docs.
	humaConfig := huma.DefaultConfig("PushWard Relay", "1.0.0")
	humaConfig.Info.Description = "Webhook relay that bridges external service webhooks to PushWard push notifications"
	humaConfig.AllowAdditionalPropertiesByDefault = true
	humaConfig.FieldsOptionalByDefault = true
	humaConfig.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearerAuth": {
			Type:   "http",
			Scheme: "bearer",
			Description: "PushWard integration key (hlk_...). " +
				"Pass via Authorization: Bearer hlk_... or HTTP Basic Auth with the key as password.",
		},
	}
	api := humago.New(mux, humaConfig)
	api.UseMiddleware(humautil.IPRateLimitMiddleware(api))
	api.UseMiddleware(humautil.AuthMiddleware(api))
	api.UseMiddleware(humautil.KeyRateLimitMiddleware(api))

	// Provider handlers
	var enders []*lifecycle.Ender

	// collectEnder appends the handler's Ender if it implements lifecycle.EnderProvider.
	collectEnder := func(handler any) {
		if ep, ok := handler.(lifecycle.EnderProvider); ok {
			enders = append(enders, ep.Ender())
		}
	}

	if cfg.Providers.Grafana.Enabled {
		grafana.RegisterRoutes(api, store, clients, &cfg.Providers.Grafana)
		slog.Info("enabled provider", "provider", "grafana")
	}

	if cfg.Providers.ArgoCD.Enabled {
		ah := argocd.RegisterRoutes(api, store, clients, &cfg.Providers.ArgoCD)
		collectEnder(ah)
		ah.StartCleanup(ctx)
		ah.RecoverPending(ctx)
		slog.Info("enabled provider", "provider", "argocd")
	}

	if cfg.Providers.Starr.Enabled {
		sh := starr.RegisterRoutes(api, store, clients, &cfg.Providers.Starr)
		collectEnder(sh)
		slog.Info("enabled provider", "provider", "starr")
	}

	if cfg.Providers.Jellyfin.Enabled {
		jh := jellyfin.RegisterRoutes(api, store, clients, &cfg.Providers.Jellyfin)
		collectEnder(jh)
		jh.StartCleanup(ctx)
		slog.Info("enabled provider", "provider", "jellyfin")
	}

	if cfg.Providers.Paperless.Enabled {
		ph := paperless.RegisterRoutes(api, store, clients, &cfg.Providers.Paperless)
		collectEnder(ph)
		slog.Info("enabled provider", "provider", "paperless")
	}

	if cfg.Providers.Changedetection.Enabled {
		changedetection.RegisterRoutes(api, clients, &cfg.Providers.Changedetection)
		slog.Info("enabled provider", "provider", "changedetection")
	}

	if cfg.Providers.Unmanic.Enabled {
		uh := unmanic.RegisterRoutes(api, clients, &cfg.Providers.Unmanic)
		collectEnder(uh)
		slog.Info("enabled provider", "provider", "unmanic")
	}

	if cfg.Providers.Bazarr.Enabled {
		bazarr.RegisterRoutes(api, clients, &cfg.Providers.Bazarr)
		slog.Info("enabled provider", "provider", "bazarr")
	}

	if cfg.Providers.Proxmox.Enabled {
		pxh := proxmox.RegisterRoutes(api, store, clients, &cfg.Providers.Proxmox)
		collectEnder(pxh)
		slog.Info("enabled provider", "provider", "proxmox")
	}

	if cfg.Providers.Overseerr.Enabled {
		oh := overseerr.RegisterRoutes(api, store, clients, &cfg.Providers.Overseerr)
		collectEnder(oh)
		slog.Info("enabled provider", "provider", "overseerr")
	}

	if cfg.Providers.UptimeKuma.Enabled {
		ukh := uptimekuma.RegisterRoutes(api, store, clients, &cfg.Providers.UptimeKuma)
		collectEnder(ukh)
		slog.Info("enabled provider", "provider", "uptimekuma")
	}

	if cfg.Providers.Gatus.Enabled {
		gah := gatus.RegisterRoutes(api, store, clients, &cfg.Providers.Gatus)
		collectEnder(gah)
		slog.Info("enabled provider", "provider", "gatus")
	}

	if cfg.Providers.Backrest.Enabled {
		bh := backrest.RegisterRoutes(api, store, clients, &cfg.Providers.Backrest)
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
				if n := ratelimit.SweepStale(5 * time.Minute); n > 0 {
					slog.Debug("rate limiter sweep", "removed", n)
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

	// Flush all pending ender timers (send ENDED immediately), then wait for in-flight callbacks.
	for _, e := range enders {
		e.FlushAll()
	}
	for _, e := range enders {
		e.Wait()
	}

	slog.Info("shutdown complete")
}
