package sabnzbd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetQueue_Success(t *testing.T) {
	expected := Queue{
		Status:   "Downloading",
		MB:       "1024",
		MBLeft:   "512",
		KBPerSec: "51200",
		TimeLeft: "0:00:10",
		Slots: []QueueSlot{
			{Filename: "ubuntu-24.04.nzb"},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("mode") != "queue" {
			t.Errorf("expected mode=queue, got %s", r.URL.Query().Get("mode"))
		}
		if r.URL.Query().Get("apikey") != "test-key" {
			t.Errorf("expected apikey=test-key, got %s", r.URL.Query().Get("apikey"))
		}
		if r.URL.Query().Get("output") != "json" {
			t.Errorf("expected output=json, got %s", r.URL.Query().Get("output"))
		}
		json.NewEncoder(w).Encode(QueueResponse{Queue: expected})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-key")
	queue, err := client.GetQueue(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if queue.Status != expected.Status {
		t.Errorf("status: got %q, want %q", queue.Status, expected.Status)
	}
	if queue.MB != expected.MB {
		t.Errorf("MB: got %q, want %q", queue.MB, expected.MB)
	}
	if queue.MBLeft != expected.MBLeft {
		t.Errorf("MBLeft: got %q, want %q", queue.MBLeft, expected.MBLeft)
	}
	if len(queue.Slots) != 1 {
		t.Fatalf("expected 1 slot, got %d", len(queue.Slots))
	}
	if queue.Slots[0].Filename != "ubuntu-24.04.nzb" {
		t.Errorf("filename: got %q, want %q", queue.Slots[0].Filename, "ubuntu-24.04.nzb")
	}
}

func TestGetQueue_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-key")
	_, err := client.GetQueue(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestGetQueue_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-key")
	_, err := client.GetQueue(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestGetQueue_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(QueueResponse{})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-key")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := client.GetQueue(ctx)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestGetHistory_Success(t *testing.T) {
	expected := History{
		Slots: []HistorySlot{
			{Status: "Completed", Name: "ubuntu-24.04", Bytes: 524288000, DownloadTime: 10},
			{Status: "Extracting", Name: "fedora-40"},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("mode") != "history" {
			t.Errorf("expected mode=history, got %s", r.URL.Query().Get("mode"))
		}
		if r.URL.Query().Get("limit") != "5" {
			t.Errorf("expected limit=5, got %s", r.URL.Query().Get("limit"))
		}
		json.NewEncoder(w).Encode(HistoryResponse{History: expected})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-key")
	history, err := client.GetHistory(context.Background(), 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(history.Slots) != 2 {
		t.Fatalf("expected 2 slots, got %d", len(history.Slots))
	}
	if history.Slots[0].Status != "Completed" {
		t.Errorf("slot 0 status: got %q, want %q", history.Slots[0].Status, "Completed")
	}
	if history.Slots[1].Name != "fedora-40" {
		t.Errorf("slot 1 name: got %q, want %q", history.Slots[1].Name, "fedora-40")
	}
}

func TestGetHistory_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-key")
	_, err := client.GetHistory(context.Background(), 5)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestGetHistory_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(HistoryResponse{})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-key")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.GetHistory(ctx, 5)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestNewClient_SetsFields(t *testing.T) {
	client := NewClient("http://localhost:8080", "my-api-key")
	if client.baseURL != "http://localhost:8080" {
		t.Errorf("baseURL: got %q, want %q", client.baseURL, "http://localhost:8080")
	}
	if client.apiKey != "my-api-key" {
		t.Errorf("apiKey: got %q, want %q", client.apiKey, "my-api-key")
	}
	if client.httpClient == nil {
		t.Fatal("httpClient should not be nil")
	}
}
