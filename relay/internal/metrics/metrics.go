package metrics

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/httputil"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pushward_relay",
		Name:      "http_requests_total",
		Help:      "Total number of HTTP requests by method, route, and status code.",
	}, []string{"method", "route", "status_code"})

	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "pushward_relay",
		Name:      "http_request_duration_seconds",
		Help:      "HTTP request duration in seconds by method and route.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"method", "route"})

	httpRequestsInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "pushward_relay",
		Name:      "http_requests_in_flight",
		Help:      "Number of HTTP requests currently being processed.",
	})

	DBPoolTotalConns = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "pushward_relay",
		Subsystem: "db_pool",
		Name:      "total_conns",
		Help:      "Total number of connections in the pool.",
	})

	DBPoolIdleConns = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "pushward_relay",
		Subsystem: "db_pool",
		Name:      "idle_conns",
		Help:      "Number of idle connections in the pool.",
	})

	DBPoolAcquiredConns = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "pushward_relay",
		Subsystem: "db_pool",
		Name:      "acquired_conns",
		Help:      "Number of currently acquired connections.",
	})

	APICallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pushward_relay",
		Name:      "api_calls_total",
		Help:      "Total PushWard API calls by provider, operation, and result.",
	}, []string{"provider", "operation", "result"})

	// WebhookIgnoredTotal makes a misconfigured sender measurable. The response
	// carries the reason in its body and the request is a plain 200, so without
	// this the condition is invisible to http_requests_total. Reason must come
	// from the handler's closed set, never from payload content, or it becomes an
	// unbounded label dimension.
	WebhookIgnoredTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pushward_relay",
		Name:      "webhook_ignored_total",
		Help:      "Webhooks accepted but not acted on in full, by provider and reason.",
	}, []string{"provider", "reason"})

	APICallRetriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pushward_relay",
		Name:      "api_call_retries_total",
		Help:      "Total retries for PushWard API calls.",
	}, []string{"provider", "operation"})

	APICallDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "pushward_relay",
		Name:      "api_call_duration_seconds",
		Help:      "Duration of PushWard API calls including retries.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"provider", "operation"})

	// PosterFetchTotal counts artwork fetches by outcome. "refused" and
	// "timeout" are ordinary against a LAN media server; a sustained shift into
	// "error" is the signal, and the result set is closed so it stays a usable
	// label.
	PosterFetchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pushward_relay",
		Name:      "poster_fetch_total",
		Help:      "Artwork fetches for activity thumbhashes, by outcome.",
	}, []string{"result"})

	// RootDispatchTotal counts webhooks posted to / by where they went and
	// why. route is a provider route, /universal, or / for one left to 404;
	// via is header or body (detected), disabled (detected, but that
	// provider is off), veto, none, or skipped (body not inspected: no key,
	// not JSON, IP over its limit, 1 MiB or more, a failed read, or a client
	// gone before detection ran). Both sets are closed.
	RootDispatchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pushward_relay",
		Name:      "root_dispatch_total",
		Help:      "Webhooks posted to the root path, by the route they were dispatched to and why.",
	}, []string{"route", "via"})

	CircuitBreakerOpen = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "pushward_relay",
		Name:      "circuit_breaker_open",
		Help:      "Whether the circuit breaker is open (1) or closed (0).",
	})
)

// Universal webhook metrics. Every label takes values from a closed set in
// relay code; the ?source= slug is caller-chosen, so it is never a label.
var (
	// UniversalEventsTotal counts universal webhooks delivered. via is where
	// the mapping came from: preset, or proposer for the plain notification
	// a payload no preset knows becomes. kind is notification, alert or
	// progress.
	UniversalEventsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pushward_relay",
		Subsystem: "universal",
		Name:      "events_total",
		Help:      "Universal webhooks delivered, by where the mapping came from and kind.",
	}, []string{"via", "kind"})

	// UniversalProposerFallbackTotal counts proposals the primary proposer
	// could not make, by universal.Fallback's reason.
	UniversalProposerFallbackTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pushward_relay",
		Subsystem: "universal",
		Name:      "proposer_fallback_total",
		Help:      "Universal proposals that fell back to the heuristic, by reason.",
	}, []string{"reason"})

	// UniversalValueFallbackTotal counts severity and lifecycle values a
	// mapping's table did not cover, by role and where the value came from
	// instead (heuristic or default).
	UniversalValueFallbackTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "pushward_relay",
		Subsystem: "universal",
		Name:      "value_fallback_total",
		Help:      "Universal severity and lifecycle values missing from the mapping table, by role and fallback.",
	}, []string{"role", "from"})
)

// Handler returns the Prometheus metrics HTTP handler.
func Handler() http.Handler {
	return promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{
		MaxRequestsInFlight: 3,
	})
}

// RecordAPICall records PushWard API call metrics from a ResultInfo callback.
func RecordAPICall(ctx context.Context, info pushward.ResultInfo) {
	provider := ProviderFromContext(ctx)
	result := "success"
	if info.Err != nil {
		result = "failed"
	}
	APICallsTotal.WithLabelValues(provider, info.Operation, result).Inc()
	if info.Attempts > 1 {
		APICallRetriesTotal.WithLabelValues(provider, info.Operation).Add(float64(info.Attempts - 1))
	}
	APICallDuration.WithLabelValues(provider, info.Operation).Observe(info.Duration.Seconds())
}

// Middleware records HTTP request metrics (duration, count, in-flight).
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/ready" {
			next.ServeHTTP(w, r)
			return
		}

		httpRequestsInFlight.Inc()
		defer httpRequestsInFlight.Dec()

		rw := httputil.NewResponseCapture(w)
		start := time.Now()

		next.ServeHTTP(rw, r)

		duration := time.Since(start).Seconds()

		route := r.Pattern
		if route == "" {
			route = "unknown"
		}

		// Sanitize the method: net/http accepts any RFC 9110 token, so an
		// unsanitized r.Method is an unbounded Prometheus label dimension that an
		// attacker can use to grow the registry without bound (CVE-2022-21698).
		method := sanitizeMethod(r.Method)
		httpRequestDuration.WithLabelValues(method, route).Observe(duration)
		httpRequestsTotal.WithLabelValues(method, route, strconv.Itoa(rw.Status)).Inc()
	})
}

// sanitizeMethod caps the HTTP method label to the known set plus "other",
// mirroring promhttp's sanitizeMethod, so attacker-controlled method tokens
// cannot inflate metric cardinality.
func sanitizeMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions,
		http.MethodConnect, http.MethodTrace:
		return m
	default:
		return "other"
	}
}
