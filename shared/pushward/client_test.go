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

// --- throttleDelay ---

func TestThrottleDelay(t *testing.T) {
	tests := []struct {
		name   string
		header string
		ms     int64
		want   time.Duration
	}{
		{name: "no hint falls back to exponential backoff", header: "", ms: 0, want: 0},
		{name: "header wins over problem body", header: "5", ms: 99000, want: 5 * time.Second},
		{name: "problem body used when header absent", header: "", ms: 1500, want: 1500 * time.Millisecond},
		{name: "header clamped to maxRetryAfter", header: "1209600", ms: 0, want: maxRetryAfter},
		{name: "problem body clamped to maxRetryAfter", header: "", ms: 1209600000, want: maxRetryAfter},
		{name: "garbage header falls through to body", header: "soon", ms: 2000, want: 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := throttleDelay(tt.header, tt.ms); got != tt.want {
				t.Errorf("throttleDelay(%q, %d) = %v, want %v", tt.header, tt.ms, got, tt.want)
			}
		})
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
	if !bytes.Contains(gotBody, []byte(`"state":"ended"`)) {
		t.Errorf("expected state:ended in body, got %s", string(gotBody))
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

// A half-open probe that resolves to a non-retryable 4xx proves the backend is
// reachable, so it must close the breaker rather than leave it wedged in
// half-open forever (regression for breaker-half-open-wedge).
func TestDoWithRetry_HalfOpenProbe_ClientErrorClosesBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cb := NewCircuitBreaker(1, 5*time.Second)
	cb.now = func() time.Time { return now }
	cb.RecordFailure() // open
	now = now.Add(6 * time.Second)

	c := NewClient(srv.URL, "key", WithCircuitBreaker(cb))
	err := c.doWithRetry(context.Background(), "create", http.MethodPost, srv.URL+"/test", "", nil, nil)
	if err == nil {
		t.Fatal("expected 4xx error from probe")
	}
	if cb.IsOpen() {
		t.Fatal("breaker wedged: a 4xx probe should close the breaker (backend reachable)")
	}
	if !cb.Allow() {
		t.Error("expected closed breaker to allow subsequent requests")
	}
}

// A half-open probe resolving to a handled 409 also proves reachability and
// must close the breaker (regression for breaker-half-open-wedge).
func TestDoWithRetry_HalfOpenProbe_ConflictClosesBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cb := NewCircuitBreaker(1, 5*time.Second)
	cb.now = func() time.Time { return now }
	cb.RecordFailure() // open
	now = now.Add(6 * time.Second)

	c := NewClient(srv.URL, "key", WithCircuitBreaker(cb))
	err := c.doWithRetry(context.Background(), "create", http.MethodPost, srv.URL+"/test", "", nil,
		func([]byte) (bool, error) { return true, errors.New("limit reached") })
	if err == nil {
		t.Fatal("expected handled-conflict error")
	}
	if cb.IsOpen() {
		t.Fatal("breaker wedged: a handled 409 probe should close the breaker")
	}
}

// Sustained 429 throttling is backpressure from a reachable backend, not a
// health fault — exhausting retries on 429 must NOT open the breaker, and the
// returned error must be a typed *HTTPError carrying status 429 (regressions
// for breaker-429-counts-as-failure and client-429-untyped-error).
func TestDoWithRetry_429Exhaustion_DoesNotTripBreaker_TypedError(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		// Tiny retry_after_ms keeps the 5-attempt retry loop fast.
		_, _ = w.Write([]byte(`{"retry_after_ms":1}`))
	}))
	defer srv.Close()

	cb := NewCircuitBreaker(1, time.Minute) // threshold 1: any RecordFailure opens it
	c := NewClient(srv.URL, "key", WithCircuitBreaker(cb))
	err := c.doWithRetry(context.Background(), "create", http.MethodPost, srv.URL+"/test", "", nil, nil)
	if err == nil {
		t.Fatal("expected error after 429 exhaustion")
	}
	if cb.IsOpen() {
		t.Error("429 throttling must not trip the circuit breaker")
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected a typed *HTTPError from 429 exhaustion, got %T: %v", err, err)
	}
	if he.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected status 429 on the typed error, got %d", he.StatusCode)
	}
	if count.Load() != 5 {
		t.Errorf("expected 5 attempts before exhaustion, got %d", count.Load())
	}
}

// A half-open probe whose context is cancelled mid-backoff (before the request
// reaches the backend) must NOT leave the breaker wedged in half-open. The
// cancel path calls breaker.Abort(), re-arming it to open with a fresh cooldown:
// Allow() is false immediately, then true once that cooldown elapses (regression
// for breaker-half-open-wedge on ctx cancel during retry backoff).
func TestDoWithRetry_ContextCancelDuringBackoff_DoesNotWedgeHalfOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // 5xx → retryable, schedules a backoff
	}))
	defer srv.Close()

	const cooldown = 5 * time.Second
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cb := NewCircuitBreaker(1, cooldown)
	cb.now = func() time.Time { return now }
	cb.RecordFailure()                    // opens the breaker
	now = now.Add(cooldown + time.Second) // past cooldown: next Allow half-opens

	c := NewClient(srv.URL, "key", WithCircuitBreaker(cb))

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel during the first retry backoff (min backoff is ~500ms): the probe
	// is admitted, the first 503 schedules a retry, and the cancel lands inside
	// that backoff sleep before the request reaches the backend again.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := c.doWithRetry(ctx, "create", http.MethodPost, srv.URL+"/test", "", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// Re-armed open: rejects immediately, admits a fresh probe after the cooldown.
	if cb.Allow() {
		t.Error("expected breaker to reject immediately after the aborted probe (re-armed open)")
	}
	now = now.Add(cooldown + time.Second)
	if !cb.Allow() {
		t.Error("breaker wedged half-open: a new probe must be admitted after the re-armed cooldown")
	}
}

// An interleaved non-retryable 4xx must NOT reset the closed-state fault streak.
// doWithRetry classifies a 4xx as breakerReachable (RecordReachable), not a
// success (RecordSuccess), so a climbing streak of real faults survives the 4xx
// and still opens the breaker (regression for the shared relay-wide breaker that
// routine per-tenant 4xx would otherwise perpetually reset).
func TestDoWithRetry_InterleavedClientError_DoesNotResetFaultStreak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // non-retryable 4xx
	}))
	defer srv.Close()

	cb := NewCircuitBreaker(2, time.Minute)
	c := NewClient(srv.URL, "key", WithCircuitBreaker(cb))

	cb.RecordFailure() // a real 5xx/network fault already on the streak (failures=1)
	if cb.IsOpen() {
		t.Fatal("breaker should still be closed after 1 fault (threshold 2)")
	}

	// The 4xx goes through doWithRetry — the path under test. It must be a typed
	// HTTPError (proving it took the 4xx branch) and must not reset the streak.
	err := c.doWithRetry(context.Background(), "create", http.MethodPost, srv.URL+"/test", "", nil, nil)
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected a typed *HTTPError with status 400, got %T: %v", err, err)
	}
	if cb.IsOpen() {
		t.Fatal("a single 4xx must not open the breaker")
	}

	// The second genuine fault hits the threshold — but only if the interleaved
	// 4xx left the streak intact. Had the 4xx been recorded as a success, the
	// streak would have reset to 0 and this would leave the breaker closed.
	cb.RecordFailure()
	if !cb.IsOpen() {
		t.Error("breaker must open: the interleaved 4xx must not have reset the fault streak (RecordReachable, not RecordSuccess)")
	}
}

// --- board / log / tap-action wire shape ---

func TestPatchActivity_BoardTilesAndTapActions(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		if r.URL.Path != "/activities/board-app" {
			t.Errorf("expected /activities/board-app, got %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	err := c.PatchActivity(context.Background(), "board-app", PatchRequest{
		State: StateOngoing,
		Content: &ContentPatch{
			Template:  StringPtr(TemplateBoard),
			TapAction: &TapAction{URL: "pushward://activity"},
			Tiles: []BoardTile{{
				Label:     "Living Room",
				Value:     "21.5",
				Unit:      "°C",
				Trend:     TrendUp,
				URLAction: &TapAction{URL: "https://ha.local/toggle", Method: "POST"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	content, ok := got["content"].(map[string]any)
	if !ok {
		t.Fatalf("no content object in body: %v", got)
	}
	if content["template"] != "board" {
		t.Errorf("expected template board, got %v", content["template"])
	}
	if tap, ok := content["tap_action"].(map[string]any); !ok || tap["url"] != "pushward://activity" {
		t.Errorf("tap_action wrong: %v", content["tap_action"])
	}
	tiles, ok := content["tiles"].([]any)
	if !ok || len(tiles) != 1 {
		t.Fatalf("expected 1 tile, got %v", content["tiles"])
	}
	tile := tiles[0].(map[string]any)
	if tile["label"] != "Living Room" || tile["value"] != "21.5" || tile["unit"] != "°C" || tile["trend"] != "up" {
		t.Errorf("tile fields wrong: %v", tile)
	}
	ua, ok := tile["url_action"].(map[string]any)
	if !ok || ua["url"] != "https://ha.local/toggle" || ua["method"] != "POST" {
		t.Errorf("tile url_action wrong: %v", tile["url_action"])
	}
}

func TestUpdateActivity_LogLines(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	at := int64(1800000000)
	c := NewClient(srv.URL, "hlk_test")
	err := c.UpdateActivity(context.Background(), "log-app", UpdateRequest{
		State: StateOngoing,
		Content: Content{
			Template: TemplateLog,
			Lines:    []LogLine{{Text: "build started", Level: LogInfo, At: &at}},
		},
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	content, ok := got["content"].(map[string]any)
	if !ok {
		t.Fatalf("no content object in body: %v", got)
	}
	if content["template"] != "log" {
		t.Errorf("expected template log, got %v", content["template"])
	}
	lines, ok := content["lines"].([]any)
	if !ok || len(lines) != 1 {
		t.Fatalf("expected 1 line, got %v", content["lines"])
	}
	line := lines[0].(map[string]any)
	if line["text"] != "build started" || line["level"] != "info" {
		t.Errorf("line fields wrong: %v", line)
	}
	if line["at"].(float64) != float64(at) {
		t.Errorf("line at wrong: %v", line["at"])
	}
}

func TestUpdateWidget_TapActionSlots(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	err := c.UpdateWidget(context.Background(), "cpu", UpdateWidgetRequest{
		Content: &WidgetContent{
			Template:  WidgetTemplateValue,
			TapAction: &TapAction{URL: "https://grafana.local/d/abc", Foreground: true},
		},
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	content, ok := got["content"].(map[string]any)
	if !ok {
		t.Fatalf("no content object in body: %v", got)
	}
	tap, ok := content["tap_action"].(map[string]any)
	if !ok || tap["url"] != "https://grafana.local/d/abc" || tap["foreground"] != true {
		t.Errorf("widget tap_action wrong: %v", content["tap_action"])
	}
}

// --- quota vs rate limit: two very different 429s ---

// quota.exceeded is a sticky monthly cap. Retrying inside a single call can
// never succeed, and honoring the (multi-week) Retry-After would park the
// caller for maxRetryAfter on every remaining attempt. Regression guard: the
// client must return after exactly one request, with the quota detail attached,
// and must not trip the breaker.
func TestDoWithRetry_QuotaExceeded_FailsFast(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("Retry-After", "1209600") // 14 days
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"title":"Too Many Requests","status":429,` +
			`"detail":"Monthly notification quota exceeded for the free tier.",` +
			`"code":"quota.exceeded","kind":"notifications","used":501,"limit":500,` +
			`"reset_at":"2026-08-01T00:00:00Z","retry_after_ms":1209600000}`))
	}))
	defer srv.Close()

	cb := NewCircuitBreaker(1, time.Minute) // threshold 1: any RecordFailure opens it
	c := NewClient(srv.URL, "key", WithCircuitBreaker(cb))

	err := c.doWithRetry(context.Background(), "notify", http.MethodPost, srv.URL+"/notifications", "", nil, nil)

	if err == nil {
		t.Fatal("expected an error on quota.exceeded")
	}
	// One attempt proves no backoff sleep ran: every retry path sleeps first.
	if got := count.Load(); got != 1 {
		t.Errorf("quota.exceeded must not be retried: expected 1 attempt, got %d", got)
	}
	if cb.IsOpen() {
		t.Error("quota.exceeded is a reachable backend and must not trip the circuit breaker")
	}

	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected a typed *HTTPError, got %T: %v", err, err)
	}
	if he.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected status 429, got %d", he.StatusCode)
	}
	if he.Code != ErrCodeQuotaExceeded {
		t.Errorf("expected code %q, got %q", ErrCodeQuotaExceeded, he.Code)
	}
	if he.Kind != "notifications" {
		t.Errorf("expected kind notifications, got %q", he.Kind)
	}
	if he.Used != 501 || he.Limit != 500 {
		t.Errorf("expected used/limit 501/500, got %d/%d", he.Used, he.Limit)
	}
	if he.ResetAt != "2026-08-01T00:00:00Z" {
		t.Errorf("expected reset_at 2026-08-01T00:00:00Z, got %q", he.ResetAt)
	}
}

// rate_limit.exceeded is transient IP backpressure: still retried, and still
// classified as reachable rather than a breaker fault.
func TestDoWithRetry_RateLimitExceeded_StillRetries(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusTooManyRequests)
		// Tiny retry_after_ms keeps the 5-attempt retry loop fast.
		_, _ = w.Write([]byte(`{"code":"rate_limit.exceeded","retry_after_ms":1}`))
	}))
	defer srv.Close()

	cb := NewCircuitBreaker(1, time.Minute)
	c := NewClient(srv.URL, "key", WithCircuitBreaker(cb))
	err := c.doWithRetry(context.Background(), "create", http.MethodPost, srv.URL+"/test", "", nil, nil)
	if err == nil {
		t.Fatal("expected error after 429 exhaustion")
	}
	if got := count.Load(); got != 5 {
		t.Errorf("expected 5 attempts before exhaustion, got %d", got)
	}
	if cb.IsOpen() {
		t.Error("rate limiting must not trip the circuit breaker")
	}
}

// --- PatchActivity upsert opt-in ---

func TestPatchActivity_UpsertQueryParam(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opts       []PatchOption
		wantUpsert bool
	}{
		{name: "default sends no upsert param", opts: nil, wantUpsert: false},
		{name: "WithUpsert sends upsert=true", opts: []PatchOption{WithUpsert()}, wantUpsert: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotUpsert string
			var hasUpsert bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotUpsert = r.URL.Query().Get("upsert")
				hasUpsert = r.URL.Query().Has("upsert")
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "hlk_test")
			if err := c.PatchActivity(context.Background(), "deploy", PatchRequest{State: StateOngoing}, tc.opts...); err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
			if gotPath != "/activities/deploy" {
				t.Errorf("expected path /activities/deploy, got %s", gotPath)
			}
			switch {
			case tc.wantUpsert && gotUpsert != "true":
				t.Errorf("expected upsert=true, got %q", gotUpsert)
			case !tc.wantUpsert && hasUpsert:
				t.Errorf("expected no upsert param, got %q", gotUpsert)
			}
		})
	}
}

// A slug carrying a "?" must not graft a query parameter onto the request: an
// unescaped slug would let a caller turn a plain patch into an upsert.
func TestPatchActivity_EscapesSlugInPath(t *testing.T) {
	var gotPath string
	var hasUpsert bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		hasUpsert = r.URL.Query().Has("upsert")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	if err := c.PatchActivity(context.Background(), "evil?upsert=true", PatchRequest{State: StateOngoing}); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if hasUpsert {
		t.Error("slug must not be able to inject an upsert query param")
	}
	if gotPath != "/activities/evil?upsert=true" {
		t.Errorf("expected the whole slug to stay one path segment, got %q", gotPath)
	}
}

// --- notification wire shape: activity_slug + silent-webhook actions ---

func TestSendNotification_ActivitySlugAndSilentAction(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/notifications" {
			t.Errorf("expected /notifications, got %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	err := c.SendNotification(context.Background(), SendNotificationRequest{
		Title:        "Deploy failed",
		Body:         "rollout aborted",
		ActivitySlug: "deploy-prod",
		Actions: []NotificationAction{{
			ID:      "ack",
			Title:   "Acknowledge",
			URL:     "https://hooks.example.com/ack",
			Method:  "POST",
			Headers: map[string]string{"Authorization": "Bearer tok"},
			Body:    `{"acked":true}`,
		}},
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	if got["activity_slug"] != "deploy-prod" {
		t.Errorf("expected activity_slug deploy-prod, got %v", got["activity_slug"])
	}
	actions, ok := got["actions"].([]any)
	if !ok || len(actions) != 1 {
		t.Fatalf("expected 1 action, got %v", got["actions"])
	}
	action, ok := actions[0].(map[string]any)
	if !ok {
		t.Fatalf("action is not an object: %v", actions[0])
	}
	if action["method"] != "POST" {
		t.Errorf("expected method POST, got %v", action["method"])
	}
	if action["body"] != `{"acked":true}` {
		t.Errorf("expected body to round-trip, got %v", action["body"])
	}
	headers, ok := action["headers"].(map[string]any)
	if !ok || headers["Authorization"] != "Bearer tok" {
		t.Errorf("expected headers to round-trip, got %v", action["headers"])
	}
}

// A notification that sets none of the optional routing fields must not emit
// them: activity_slug and the action's method/headers/body all carry omitempty,
// so an unset action stays a plain button rather than a silent webhook.
func TestSendNotification_OmitsUnsetOptionalFields(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	err := c.SendNotification(context.Background(), SendNotificationRequest{
		Title:   "Plain",
		Body:    "no routing",
		Actions: []NotificationAction{{ID: "open", Title: "Open"}},
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	if _, present := got["activity_slug"]; present {
		t.Errorf("activity_slug must be omitted when unset, got %v", got["activity_slug"])
	}
	// The whole point of Push being *bool: a nil Push must omit the key so the
	// server applies its default of true. If this key ever reappears as false,
	// every caller that leaves Push unset silently stops pushing.
	if _, present := got["push"]; present {
		t.Errorf("push must be omitted when Push is nil, got %v", got["push"])
	}
	action := got["actions"].([]any)[0].(map[string]any)
	for _, key := range []string{"method", "headers", "body", "url"} {
		if _, present := action[key]; present {
			t.Errorf("action key %q must be omitted when unset, got %v", key, action[key])
		}
	}
}
