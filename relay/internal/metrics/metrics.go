package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/httputil"
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
)

// Handler returns the Prometheus metrics HTTP handler.
func Handler() http.Handler {
	return promhttp.Handler()
}

// Middleware records HTTP request metrics (duration, count, in-flight).
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" || r.URL.Path == "/health" || r.URL.Path == "/ready" {
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

		httpRequestDuration.WithLabelValues(r.Method, route).Observe(duration)
		httpRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(rw.Status)).Inc()
	})
}
