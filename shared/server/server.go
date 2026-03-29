package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// NewMux creates an http.ServeMux with /health (liveness) and /ready (readiness) endpoints.
// The optional checks are called on /ready requests; if any returns an error, /ready responds 503.
func NewMux(checks ...func(context.Context) error) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for _, check := range checks {
			if err := check(ctx); err != nil {
				slog.Warn("readiness check failed", "error", err)
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
	})
	return mux
}

// ListenAndServe starts an HTTP server and blocks until ctx is cancelled.
// It performs a graceful shutdown with a 5-second timeout.
func ListenAndServe(ctx context.Context, addr string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
