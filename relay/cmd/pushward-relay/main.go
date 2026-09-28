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
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/ratelimit"
	"github.com/mac-lucky/pushward-integrations/relay/internal/rootroute"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/telemetry"
	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/poster"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/server"
	"github.com/mac-lucky/pushward-integrations/shared/syncx"
)

func main() {
	configPath := flag.String("config", "config.yml", "path to config file")
	pushwardURL := flag.String("pushward-url", "", "PushWard server URL (overrides config)")
	flag.Parse()

	logger := sharedconfig.NewLogger()
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
	} else {
		// Without trusted proxies, ClientIP falls back to the socket peer - which
		// behind a reverse proxy (Cloudflare/gateway) is the SAME proxy IP for
		// every request, collapsing all traffic into one per-IP bucket (5 r/s).
		slog.Warn("no trusted_proxy_cidrs configured: per-IP rate limiting will collapse all reverse-proxied traffic into a single bucket; set PUSHWARD_TRUSTED_PROXY_CIDRS if running behind Cloudflare/a gateway")
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

	// Client pool - use an instrumented transport when tracing is enabled
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

	// Huma API - auto-generates OpenAPI 3.1 spec at /openapi.json and docs at /docs.
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
	api.UseMiddleware(humautil.OverridesMiddleware(api))
	api.UseMiddleware(humautil.KeyRateLimitMiddleware(api))

	// Poster image resolution. Providers hold a poster.Source unconditionally,
	// so turning the feature off swaps in a no-op rather than adding a branch to
	// every content build.
	var posters poster.Source = poster.Disabled{}
	if cfg.Poster.IsEnabled() {
		posters = poster.NewResolver(poster.Config{
			AllowPrivateHosts: cfg.Poster.AllowPrivateHosts,
			FetchTimeout:      cfg.Poster.FetchTimeout,
			InlineWait:        cfg.Poster.InlineWait,
			OnResult: func(result string) {
				metrics.PosterFetchTotal.WithLabelValues(result).Inc()
			},
		})
		slog.Info("poster images enabled",
			"allow_private_hosts", cfg.Poster.AllowPrivateHosts,
			"inline_wait", cfg.Poster.InlineWait)
	} else {
		slog.Info("poster images disabled")
	}

	// Provider handlers. The store they get is wrapped for key hashing;
	// the state cleanup below uses the raw one.
	providers := registerProviders(ctx, api, store, clients, cfg, posters)

	// POST / dispatches to the route of the provider that sent the payload,
	// else to /universal. It reads which routes exist, so it comes after
	// registerProviders, and it sits inside the content-type fix so a sender
	// with no Content-Type still gets its body read.
	rootroute.Document(api)
	root := rootroute.New(mux, rootroute.Options{})

	// Wrap mux with metrics middleware and optional OTel tracing.
	handler := metrics.Middleware(humautil.NormalizeJSONContentType(root))
	if cfg.Telemetry.Endpoint != "" {
		handler = otelhttp.NewHandler(handler, "pushward-relay",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				if r.Pattern != "" {
					return r.Pattern
				}
				return r.Method
			}),
			otelhttp.WithFilter(func(r *http.Request) bool {
				return r.URL.Path != "/health" && r.URL.Path != "/ready"
			}),
		)
	}

	var stateCleanup, metricsCollect syncx.Periodic
	stateCleanup.Start(ctx, 30*time.Second, func(ctx context.Context) {
		if n, err := store.Cleanup(ctx); err != nil {
			slog.Error("state cleanup failed", "error", err)
		} else if n > 0 {
			slog.Info("state cleanup", "removed", n)
		}
		if n := ratelimit.SweepStale(5 * time.Minute); n > 0 {
			slog.Debug("rate limiter sweep", "removed", n)
		}
	})
	defer stateCleanup.Stop()

	metricsCollect.Start(ctx, 15*time.Second, func(context.Context) {
		stat := pool.Stat()
		metrics.DBPoolTotalConns.Set(float64(stat.TotalConns()))
		metrics.DBPoolIdleConns.Set(float64(stat.IdleConns()))
		metrics.DBPoolAcquiredConns.Set(float64(stat.AcquiredConns()))
		val := 0.0
		if breaker.IsOpen() {
			val = 1
		}
		metrics.CircuitBreakerOpen.Set(val)
	})
	defer metricsCollect.Stop()

	// Internal-only metrics server. Scraped via in-cluster ServiceMonitor.
	// Shut down after the main server so Prometheus can scrape final drain metrics.
	var metricsSrv *http.Server
	if cfg.Server.MetricsAddress != "" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("GET /metrics", metrics.Handler())
		metricsSrv = &http.Server{
			Addr:              cfg.Server.MetricsAddress,
			Handler:           metricsMux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		go func() {
			slog.Info("starting metrics server", "address", cfg.Server.MetricsAddress)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("metrics server failed", "error", err)
			}
		}()
		defer func() {
			metricsCtx, metricsCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer metricsCancel()
			if err := metricsSrv.Shutdown(metricsCtx); err != nil {
				slog.Error("metrics server shutdown error", "error", err)
			}
		}()
	}

	slog.Info("starting pushward-relay", "address", cfg.Server.Address)
	if err := server.ListenAndServe(ctx, cfg.Server.Address, handler); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}

	// Stop ArgoCD grace timers so none fire graceExpired mid-shutdown.
	if providers.argocd != nil {
		providers.argocd.StopAll()
	}

	// Flush all pending ender timers (send ENDED immediately), then wait for in-flight callbacks.
	for _, e := range providers.enders {
		e.FlushAll()
	}
	for _, e := range providers.enders {
		e.Wait()
	}

	slog.Info("shutdown complete")
}
