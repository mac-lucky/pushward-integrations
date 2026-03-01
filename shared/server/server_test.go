package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// --- NewMux ---

func TestNewMux_HealthEndpoint(t *testing.T) {
	mux := NewMux()

	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	resp, err := http.Get("http://" + ln.Addr().String() + "/health")
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("expected 'ok', got %q", string(body))
	}
}

func TestNewMux_UnknownRoute(t *testing.T) {
	mux := NewMux()

	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	resp, err := http.Get("http://" + ln.Addr().String() + "/nonexistent")
	if err != nil {
		t.Fatalf("GET /nonexistent failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// --- ListenAndServe ---

func TestListenAndServe_GracefulShutdown(t *testing.T) {
	mux := NewMux()
	ctx, cancel := context.WithCancel(context.Background())

	// Pick a free port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- ListenAndServe(ctx, addr, mux)
	}()

	// Wait for server to start
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Verify server is serving
	resp, err := http.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatalf("server not responding: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	// Cancel context to trigger shutdown
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected error from ListenAndServe: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe did not return after context cancellation")
	}
}

func TestListenAndServe_InvalidAddress(t *testing.T) {
	mux := NewMux()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		// Bind to an invalid address to trigger immediate error
		errCh <- ListenAndServe(ctx, "invalid-addr-that-cannot-bind:99999", mux)
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error for invalid address")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return error for invalid address")
	}
}

func TestListenAndServe_ServerTimeouts(t *testing.T) {
	// Verify the server created inside ListenAndServe respects health checks
	// (this is an integration sanity check that NewMux works with ListenAndServe)
	mux := NewMux()
	mux.HandleFunc("/custom", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("custom"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	go ListenAndServe(ctx, addr, mux)

	// Wait for server to start
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	resp, err := http.Get("http://" + addr + "/custom")
	if err != nil {
		t.Fatalf("GET /custom failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "custom" {
		t.Errorf("expected 'custom', got %q", string(body))
	}
}
