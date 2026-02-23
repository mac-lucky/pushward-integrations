package testutil

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// APICall records a PushWard API call made by a handler/poller under test.
type APICall struct {
	Method string
	Path   string
	Body   json.RawMessage
}

// MockPushWardServer starts an httptest server that records all requests.
func MockPushWardServer(t *testing.T) (*httptest.Server, *[]APICall, *sync.Mutex) {
	t.Helper()
	var calls []APICall
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, APICall{
			Method: r.Method,
			Path:   r.URL.Path,
			Body:   json.RawMessage(body),
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &mu
}

// GetCalls returns a snapshot of the recorded API calls.
func GetCalls(calls *[]APICall, mu *sync.Mutex) []APICall {
	mu.Lock()
	defer mu.Unlock()
	result := make([]APICall, len(*calls))
	copy(result, *calls)
	return result
}

// UnmarshalBody decodes the JSON body of a recorded API call into v.
func UnmarshalBody(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("failed to unmarshal body: %v (body: %s)", err, string(raw))
	}
}
