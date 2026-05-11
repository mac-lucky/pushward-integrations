package pushward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// --- parseRetryAfter ---

func TestParseRetryAfter_Empty(t *testing.T) {
	if d := parseRetryAfter(""); d != 0 {
		t.Errorf("expected 0, got %v", d)
	}
}

func TestParseRetryAfter_Seconds(t *testing.T) {
	d := parseRetryAfter("5")
	if d != 5*time.Second {
		t.Errorf("expected 5s, got %v", d)
	}
}

func TestParseRetryAfter_ZeroSeconds(t *testing.T) {
	if d := parseRetryAfter("0"); d != 0 {
		t.Errorf("expected 0 for '0', got %v", d)
	}
}

func TestParseRetryAfter_NegativeSeconds(t *testing.T) {
	if d := parseRetryAfter("-3"); d != 0 {
		t.Errorf("expected 0 for negative, got %v", d)
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	future := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	d := parseRetryAfter(future)
	if d < 5*time.Second || d > 11*time.Second {
		t.Errorf("expected ~10s for future HTTP date, got %v", d)
	}
}

func TestParseRetryAfter_PastHTTPDate(t *testing.T) {
	past := time.Now().Add(-10 * time.Second).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(past); d != 0 {
		t.Errorf("expected 0 for past date, got %v", d)
	}
}

func TestParseRetryAfter_Garbage(t *testing.T) {
	if d := parseRetryAfter("not-a-number"); d != 0 {
		t.Errorf("expected 0 for garbage input, got %v", d)
	}
}

// --- doWithRetry ---

func TestDoWithRetry_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	err := c.doWithRetry(context.Background(), "test", http.MethodGet, srv.URL+"/test", "", nil, nil)
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestDoWithRetry_SetsAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_secret")
	_ = c.doWithRetry(context.Background(), "test", http.MethodGet, srv.URL+"/test", "", nil, nil)
	if gotAuth != "Bearer hlk_secret" {
		t.Errorf("expected 'Bearer hlk_secret', got %q", gotAuth)
	}
}

func TestDoWithRetry_SetsContentType(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	_ = c.doWithRetry(context.Background(), "test", http.MethodPost, srv.URL+"/test", "", map[string]string{"a": "b"}, nil)
	if gotCT != "application/json" {
		t.Errorf("expected 'application/json', got %q", gotCT)
	}
}

func TestDoWithRetry_NoContentTypeWithoutBody(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	_ = c.doWithRetry(context.Background(), "test", http.MethodGet, srv.URL+"/test", "", nil, nil)
	if gotCT != "" {
		t.Errorf("expected empty Content-Type, got %q", gotCT)
	}
}

func TestDoWithRetry_ClientError_NoRetry(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	err := c.doWithRetry(context.Background(), "test", http.MethodGet, srv.URL+"/test", "", nil, nil)
	if err == nil {
		t.Fatal("expected error for 400")
	}
	if count.Load() != 1 {
		t.Errorf("expected 1 attempt for 4xx, got %d", count.Load())
	}
}

func TestDoWithRetry_ServerError_Retries(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	err := c.doWithRetry(context.Background(), "test", http.MethodGet, srv.URL+"/test", "", nil, nil)
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if count.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", count.Load())
	}
}

func TestDoWithRetry_429_RetriesWithRetryAfter(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	start := time.Now()
	err := c.doWithRetry(context.Background(), "test", http.MethodGet, srv.URL+"/test", "", nil, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if count.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", count.Load())
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("expected ~1s backoff from Retry-After, elapsed %v", elapsed)
	}
}

func TestDoWithRetry_Conflict_HandleConflictDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Conflict","status":409,"detail":"activity already exists","code":"activity.exists"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	err := c.doWithRetry(context.Background(), "test", http.MethodPost, srv.URL+"/test", "", nil,
		func(body []byte) (bool, error) {
			return true, nil // done, no error
		})
	if err != nil {
		t.Fatalf("expected nil from handleConflict returning done=true, got %v", err)
	}
}

func TestDoWithRetry_Conflict_HandleConflictError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Conflict","status":409,"detail":"activity limit reached","code":"activity.limit_exceeded"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	err := c.doWithRetry(context.Background(), "test", http.MethodPost, srv.URL+"/test", "", nil,
		func(body []byte) (bool, error) {
			return true, fmt.Errorf("activity limit reached")
		})
	if err == nil || err.Error() != "activity limit reached" {
		t.Fatalf("expected 'activity limit reached' error, got %v", err)
	}
}

func TestDoWithRetry_Conflict_NotDone_Retries(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n < 3 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"type":"about:blank","title":"Conflict","status":409,"detail":"transient"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	err := c.doWithRetry(context.Background(), "test", http.MethodPost, srv.URL+"/test", "", nil,
		func(body []byte) (bool, error) {
			return false, nil // not done, retry
		})
	if err != nil {
		t.Fatalf("expected success after conflict retries, got %v", err)
	}
	if count.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", count.Load())
	}
}

func TestDoWithRetry_MaxRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	err := c.doWithRetry(context.Background(), "test", http.MethodGet, srv.URL+"/test", "", nil, nil)
	if err == nil {
		t.Fatal("expected error after max retries")
	}
	if err.Error() != "max retries exceeded: unexpected status 500" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestDoWithRetry_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := NewClient(srv.URL, "key")

	// Cancel after first attempt
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := c.doWithRetry(ctx, "test", http.MethodGet, srv.URL+"/test", "", nil, nil)
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestDoWithRetry_MarshalError(t *testing.T) {
	c := NewClient("http://localhost", "key")
	// Channels cannot be marshaled to JSON
	err := c.doWithRetry(context.Background(), "test", http.MethodPost, "http://localhost/test", "", make(chan int), nil)
	if err == nil {
		t.Fatal("expected marshal error")
	}
}

// --- CreateActivity ---

func TestCreateActivity_Success(t *testing.T) {
	var gotBody CreateActivityRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/activities" {
			t.Errorf("expected /activities, got %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	err := c.CreateActivity(context.Background(), "gh-repo", "GitHub CI", 3, 900, 1800)
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if gotBody.Slug != "gh-repo" {
		t.Errorf("expected slug 'gh-repo', got %q", gotBody.Slug)
	}
	if gotBody.Name != "GitHub CI" {
		t.Errorf("expected name 'GitHub CI', got %q", gotBody.Name)
	}
	if gotBody.Priority != 3 {
		t.Errorf("expected priority 3, got %d", gotBody.Priority)
	}
	if gotBody.EndedTTL != 900 {
		t.Errorf("expected ended_ttl 900, got %d", gotBody.EndedTTL)
	}
	if gotBody.StaleTTL != 1800 {
		t.Errorf("expected stale_ttl 1800, got %d", gotBody.StaleTTL)
	}
}

func TestCreateActivity_LimitReached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Conflict","status":409,"detail":"activity limit reached","code":"activity.limit_exceeded"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	err := c.CreateActivity(context.Background(), "gh-repo", "CI", 1, 0, 0)
	if err == nil {
		t.Fatal("expected error for limit conflict")
	}
	if err.Error() != "activity limit reached" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCreateActivity_UnknownConflictSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Conflict","status":409,"detail":"unexpected","code":"activity.unexpected"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	err := c.CreateActivity(context.Background(), "gh-repo", "CI", 1, 0, 0)
	if err == nil {
		t.Fatal("expected error for unrecognised conflict")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if httpErr.Code != "activity.unexpected" {
		t.Errorf("expected code 'activity.unexpected', got %q", httpErr.Code)
	}
}

// --- UpdateActivity ---

func TestUpdateActivity_Success(t *testing.T) {
	var gotBody UpdateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		if r.URL.Path != "/activities/gh-repo" {
			t.Errorf("expected /activities/gh-repo, got %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	step := 2
	total := 4
	err := c.UpdateActivity(context.Background(), "gh-repo", UpdateRequest{
		State: StateOngoing,
		Content: Content{
			Template:    "steps",
			Progress:    0.5,
			State:       "Building",
			CurrentStep: &step,
			TotalSteps:  &total,
		},
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if gotBody.State != StateOngoing {
		t.Errorf("expected ONGOING, got %s", gotBody.State)
	}
	if gotBody.Content.Template != "steps" {
		t.Errorf("expected steps template, got %s", gotBody.Content.Template)
	}
	if gotBody.Content.Progress != 0.5 {
		t.Errorf("expected progress 0.5, got %f", gotBody.Content.Progress)
	}
	if *gotBody.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %d", *gotBody.Content.CurrentStep)
	}
}

// --- PatchActivity ---

func TestPatchActivity_Success(t *testing.T) {
	var gotMethod string
	var gotPath string
	var gotCT string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	req := PatchRequest{
		Content: &ContentPatch{
			Progress:      Float64Ptr(0.42),
			State:         StringPtr("Downloading"),
			RemainingTime: IntPtr(90),
		},
	}
	if err := c.PatchActivity(context.Background(), "gh-repo", req); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("expected PATCH, got %s", gotMethod)
	}
	if gotPath != "/activities/gh-repo" {
		t.Errorf("expected /activities/gh-repo, got %s", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("expected application/json content-type, got %q", gotCT)
	}
	content, ok := gotBody["content"].(map[string]any)
	if !ok {
		t.Fatalf("expected content object in body, got %#v", gotBody)
	}
	if content["progress"].(float64) != 0.42 {
		t.Errorf("expected progress 0.42, got %v", content["progress"])
	}
	// Absent template/icon/accent_color must not appear in the merge-patch body.
	if _, present := content["template"]; present {
		t.Error("merge-patch body must not include unset template field")
	}
	if _, present := content["icon"]; present {
		t.Error("merge-patch body must not include unset icon field")
	}
	if _, present := content["accent_color"]; present {
		t.Error("merge-patch body must not include unset accent_color field")
	}
	// State was not set on the PatchRequest, so it must also be absent.
	if _, present := gotBody["state"]; present {
		t.Error("merge-patch body must not include unset top-level state")
	}
}

func TestPatchActivity_OmitsEmptyContent(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	// State-only patch — no Content block at all.
	if err := c.PatchActivity(context.Background(), "x", PatchRequest{State: StateEnded}); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if !bytes.Contains(gotBody, []byte(`"state":"ENDED"`)) {
		t.Errorf("expected state:ENDED in body, got %s", string(gotBody))
	}
	if bytes.Contains(gotBody, []byte(`"content"`)) {
		t.Errorf("expected content field to be omitted when nil, got %s", string(gotBody))
	}
}

// --- HTTPError Problem parsing ---

func TestHTTPError_ParsesProblemBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Unprocessable Entity","status":422,"detail":"alarm requires end_date","code":"activity.alarm_requires_end_date"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	err := c.doWithRetry(context.Background(), "test", http.MethodPost, srv.URL+"/x", "", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if httpErr.StatusCode != 422 {
		t.Errorf("status: got %d, want 422", httpErr.StatusCode)
	}
	if httpErr.Code != "activity.alarm_requires_end_date" {
		t.Errorf("code: got %q", httpErr.Code)
	}
	if httpErr.Detail != "alarm requires end_date" {
		t.Errorf("detail: got %q", httpErr.Detail)
	}
	if httpErr.Title != "Unprocessable Entity" {
		t.Errorf("title: got %q", httpErr.Title)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("alarm requires end_date")) {
		t.Errorf("Error() should surface detail, got %q", err.Error())
	}
}

func TestHTTPError_LegacyErrorBodyFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"legacy shape"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	err := c.doWithRetry(context.Background(), "test", http.MethodPost, srv.URL+"/x", "", nil, nil)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if httpErr.Detail != "legacy shape" {
		t.Errorf("expected legacy error string promoted to Detail, got %q", httpErr.Detail)
	}
}

// --- UpdateRequest serialisation ---

// State has json:"state,omitempty" so a content-only update (zero State value)
// must not emit "state":"" — the new server enum rejects empty strings under
// additionalProperties:false validation.
func TestUpdateRequest_OmitsEmptyState(t *testing.T) {
	body, err := json.Marshal(UpdateRequest{
		Content: Content{Progress: 0.5},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(body, []byte(`"state"`)) {
		t.Errorf("expected no state key in body, got %s", string(body))
	}
}

// --- NewClient ---

func TestNewClient(t *testing.T) {
	c := NewClient("http://example.com", "hlk_key")
	if c.baseURL != "http://example.com" {
		t.Errorf("expected baseURL 'http://example.com', got %q", c.baseURL)
	}
	if c.apiKey != "hlk_key" {
		t.Errorf("expected apiKey 'hlk_key', got %q", c.apiKey)
	}
	if c.httpClient == nil {
		t.Error("expected non-nil httpClient")
	}
	if c.httpClient.Timeout != 10*time.Second {
		t.Errorf("expected 10s timeout, got %v", c.httpClient.Timeout)
	}
}

// --- onResult callback ---

func TestDoWithRetry_CallsOnResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var got ResultInfo
	c := NewClient(srv.URL, "key", WithOnResult(func(_ context.Context, info ResultInfo) {
		got = info
	}))
	err := c.doWithRetry(context.Background(), "create", http.MethodPost, srv.URL+"/test", "", nil, nil)
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if got.Operation != "create" {
		t.Errorf("expected operation 'create', got %q", got.Operation)
	}
	if got.Attempts != 1 {
		t.Errorf("expected 1 attempt, got %d", got.Attempts)
	}
	if got.Err != nil {
		t.Errorf("expected nil error, got %v", got.Err)
	}
	if got.Duration <= 0 {
		t.Errorf("expected positive duration, got %v", got.Duration)
	}
}

func TestDoWithRetry_OnResultReceivesRetryCount(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var got ResultInfo
	c := NewClient(srv.URL, "key", WithOnResult(func(_ context.Context, info ResultInfo) {
		got = info
	}))
	err := c.doWithRetry(context.Background(), "update", http.MethodPatch, srv.URL+"/test", "", nil, nil)
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if got.Attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", got.Attempts)
	}
	if got.Err != nil {
		t.Errorf("expected nil error after retry success, got %v", got.Err)
	}
}

func TestDoWithRetry_CircuitOpenReturnsError(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cb := NewCircuitBreaker(1, time.Minute)
	cb.RecordFailure() // trip the breaker

	var got ResultInfo
	c := NewClient(srv.URL, "key",
		WithCircuitBreaker(cb),
		WithOnResult(func(_ context.Context, info ResultInfo) {
			got = info
		}),
	)
	err := c.doWithRetry(context.Background(), "create", http.MethodPost, srv.URL+"/test", "", nil, nil)
	if err != ErrCircuitOpen {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
	if called {
		t.Error("expected no HTTP call when circuit is open")
	}
	if got.Err != ErrCircuitOpen {
		t.Errorf("expected onResult to receive ErrCircuitOpen, got %v", got.Err)
	}
	if got.Attempts != 0 {
		t.Errorf("expected 0 attempts, got %d", got.Attempts)
	}
}
