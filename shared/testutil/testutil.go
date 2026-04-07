package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// APICall records a PushWard API call made by a handler/poller under test.
type APICall struct {
	Method string
	Path   string
	Body   json.RawMessage
}

var (
	slugPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)
	hexColor    = regexp.MustCompile(`^#?[0-9a-fA-F]{6}([0-9a-fA-F]{2})?$`)
	namedColors = map[string]bool{
		"red": true, "orange": true, "yellow": true, "green": true,
		"blue": true, "purple": true, "pink": true, "indigo": true,
		"teal": true, "cyan": true, "mint": true, "brown": true,
	}
	validTemplates  = map[string]bool{"generic": true, "alert": true, "steps": true, "countdown": true, "gauge": true, "timeline": true}
	validStates     = map[string]bool{"ONGOING": true, "ENDED": true}
	validSeverities = map[string]bool{"critical": true, "warning": true, "info": true}
)

type createRequest struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Priority *int   `json:"priority,omitempty"`
	EndedTTL *int   `json:"ended_ttl,omitempty"`
	StaleTTL *int   `json:"stale_ttl,omitempty"`
}

type updateRequest struct {
	State    string     `json:"state"`
	Content  apiContent `json:"content"`
	Priority *int       `json:"priority,omitempty"`
}

type apiContent struct {
	Template          string   `json:"template"`
	Progress          float64  `json:"progress"`
	State             string   `json:"state,omitempty"`
	Icon              string   `json:"icon,omitempty"`
	Subtitle          string   `json:"subtitle,omitempty"`
	AccentColor       string   `json:"accent_color,omitempty"`
	BackgroundColor   string   `json:"background_color,omitempty"`
	TextColor         string   `json:"text_color,omitempty"`
	CurrentStep       *int     `json:"current_step,omitempty"`
	TotalSteps        *int     `json:"total_steps,omitempty"`
	StepRows          []int    `json:"step_rows,omitempty"`
	StepLabels        []string `json:"step_labels,omitempty"`
	URL               string   `json:"url,omitempty"`
	SecondaryURL      string   `json:"secondary_url,omitempty"`
	Severity          string   `json:"severity,omitempty"`
	FiredAt           *int64   `json:"fired_at,omitempty"`
	RemainingTime     *int     `json:"remaining_time,omitempty"`
	CompletionMessage string   `json:"completion_message,omitempty"`
	EndDate           *int64   `json:"end_date,omitempty"`
	StartDate         *int64   `json:"start_date,omitempty"`
	WarningThreshold  *int     `json:"warning_threshold,omitempty"`
	Value             any                `json:"value,omitempty"`
	MinValue          *float64           `json:"min_value,omitempty"`
	MaxValue          *float64           `json:"max_value,omitempty"`
	Unit              string             `json:"unit,omitempty"`
	Scale             string             `json:"scale,omitempty"`
	Decimals          *int               `json:"decimals,omitempty"`
	Smoothing         *bool              `json:"smoothing,omitempty"`
	Thresholds        []testThreshold    `json:"thresholds,omitempty"`
	Duration          *string            `json:"duration,omitempty"`
}

type testThreshold struct {
	Value float64 `json:"value"`
	Color string  `json:"color,omitempty"`
	Label string  `json:"label,omitempty"`
}

// MockPushWardServer starts an httptest server that records all requests and
// validates them against the PushWard public API contract.
func MockPushWardServer(t *testing.T) (*httptest.Server, *[]APICall, *sync.Mutex) {
	t.Helper()
	var calls []APICall
	var mu sync.Mutex
	slugs := make(map[string]bool)

	mux := http.NewServeMux()

	mux.HandleFunc("POST /activities", func(w http.ResponseWriter, r *http.Request) {
		body := recordCall(&calls, &mu, r)

		var req createRequest
		if err := json.Unmarshal(body, &req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		if err := validateCreateRequest(&req); err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}

		mu.Lock()
		defer mu.Unlock()
		if slugs[req.Slug] {
			respondError(w, http.StatusConflict, "activity already exists")
			return
		}
		slugs[req.Slug] = true
		w.WriteHeader(http.StatusCreated)
	})

	mux.HandleFunc("PATCH /activity/", func(w http.ResponseWriter, r *http.Request) {
		body := recordCall(&calls, &mu, r)

		slug := strings.TrimPrefix(r.URL.Path, "/activity/")
		if slug == "" {
			respondError(w, http.StatusBadRequest, "missing slug")
			return
		}

		var req updateRequest
		if err := json.Unmarshal(body, &req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		if err := validateUpdateRequest(&req); err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}

		mu.Lock()
		exists := slugs[slug]
		mu.Unlock()
		if !exists {
			respondError(w, http.StatusNotFound, "activity not found")
			return
		}

		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /notifications", func(w http.ResponseWriter, r *http.Request) {
		body := recordCall(&calls, &mu, r)

		var req struct {
			Title string `json:"title"`
			Body  string `json:"body"`
			Push  bool   `json:"push"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if req.Title == "" {
			respondError(w, http.StatusBadRequest, "title is required")
			return
		}
		if req.Body == "" {
			respondError(w, http.StatusBadRequest, "body is required")
			return
		}

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "pushed": req.Push})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		recordCall(&calls, &mu, r)
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
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

func recordCall(calls *[]APICall, mu *sync.Mutex, r *http.Request) json.RawMessage {
	body, _ := io.ReadAll(r.Body)
	mu.Lock()
	*calls = append(*calls, APICall{
		Method: r.Method,
		Path:   r.URL.Path,
		Body:   json.RawMessage(body),
	})
	mu.Unlock()
	return json.RawMessage(body)
}

func respondError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}


func validateCreateRequest(req *createRequest) error {
	if req.Slug == "" {
		return fmt.Errorf("slug is required")
	}
	if !slugPattern.MatchString(req.Slug) {
		return fmt.Errorf("slug must match ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$")
	}
	if req.Name == "" {
		return fmt.Errorf("name is required")
	}
	if utf8.RuneCountInString(req.Name) > 256 {
		return fmt.Errorf("name must be at most 256 runes")
	}
	if req.Priority != nil && (*req.Priority < 0 || *req.Priority > 10) {
		return fmt.Errorf("priority must be 0-10")
	}
	if req.EndedTTL != nil && (*req.EndedTTL < 1 || *req.EndedTTL > 2592000) {
		return fmt.Errorf("ended_ttl must be 1-2592000")
	}
	if req.StaleTTL != nil && (*req.StaleTTL < 1 || *req.StaleTTL > 2592000) {
		return fmt.Errorf("stale_ttl must be 1-2592000")
	}
	return nil
}

func validateUpdateRequest(req *updateRequest) error {
	if !validStates[req.State] {
		return fmt.Errorf("state must be ONGOING or ENDED")
	}
	if req.Priority != nil && (*req.Priority < 0 || *req.Priority > 10) {
		return fmt.Errorf("priority must be 0-10")
	}
	return validateContent(&req.Content)
}

func validateContent(c *apiContent) error {
	if !validTemplates[c.Template] {
		return fmt.Errorf("template must be one of: generic, alert, steps, countdown, gauge, timeline")
	}
	if c.Progress < 0 || c.Progress > 1 {
		return fmt.Errorf("progress must be 0.0-1.0")
	}
	if c.RemainingTime != nil && *c.RemainingTime < 0 {
		return fmt.Errorf("remaining_time must be >= 0")
	}
	if utf8.RuneCountInString(c.State) > 256 {
		return fmt.Errorf("content.state must be at most 256 runes")
	}
	if utf8.RuneCountInString(c.Subtitle) > 256 {
		return fmt.Errorf("subtitle must be at most 256 runes")
	}
	if utf8.RuneCountInString(c.CompletionMessage) > 512 {
		return fmt.Errorf("completion_message must be at most 512 runes")
	}
	if utf8.RuneCountInString(c.Icon) > 128 {
		return fmt.Errorf("icon must be at most 128 runes")
	}
	if err := validateURL(c.URL, "url"); err != nil {
		return err
	}
	if err := validateURL(c.SecondaryURL, "secondary_url"); err != nil {
		return err
	}
	// URL links are only supported on steps and alert templates.
	if c.Template != "steps" && c.Template != "alert" {
		if c.URL != "" {
			return fmt.Errorf("url is not supported for %q template, only for steps and alert", c.Template)
		}
		if c.SecondaryURL != "" {
			return fmt.Errorf("secondary_url is not supported for %q template, only for steps and alert", c.Template)
		}
	}
	if err := validateColor(c.AccentColor, "accent_color"); err != nil {
		return err
	}
	if err := validateColor(c.BackgroundColor, "background_color"); err != nil {
		return err
	}
	if err := validateColor(c.TextColor, "text_color"); err != nil {
		return err
	}

	switch c.Template {
	case "alert":
		if err := validateAlert(c); err != nil {
			return err
		}
	case "steps":
		if err := validateSteps(c); err != nil {
			return err
		}
	case "countdown":
		if err := validateCountdown(c); err != nil {
			return err
		}
	case "gauge":
		if err := validateGauge(c); err != nil {
			return err
		}
	case "timeline":
		if err := validateTimeline(c); err != nil {
			return err
		}
	}

	return nil
}

func validateAlert(c *apiContent) error {
	if !validSeverities[c.Severity] {
		return fmt.Errorf("severity is required and must be critical, warning, or info")
	}
	if c.FiredAt != nil && *c.FiredAt <= 0 {
		return fmt.Errorf("fired_at must be > 0")
	}
	return nil
}

func validateSteps(c *apiContent) error {
	if c.TotalSteps == nil {
		return fmt.Errorf("total_steps is required for steps template")
	}
	if *c.TotalSteps < 1 {
		return fmt.Errorf("total_steps must be >= 1")
	}
	if c.CurrentStep == nil {
		return fmt.Errorf("current_step is required for steps template")
	}
	if *c.CurrentStep < 0 || *c.CurrentStep > *c.TotalSteps {
		return fmt.Errorf("current_step must be >= 0 and <= total_steps")
	}
	if c.StepRows != nil {
		if len(c.StepRows) != *c.TotalSteps {
			return fmt.Errorf("step_rows length must equal total_steps")
		}
		for i, v := range c.StepRows {
			if v < 1 || v > 10 {
				return fmt.Errorf("step_rows[%d] must be 1-10", i)
			}
		}
	}
	if c.StepLabels != nil {
		if len(c.StepLabels) != *c.TotalSteps {
			return fmt.Errorf("step_labels length must equal total_steps")
		}
		for i, label := range c.StepLabels {
			if utf8.RuneCountInString(label) > 32 {
				return fmt.Errorf("step_labels[%d] must be at most 32 runes", i)
			}
		}
	}
	return nil
}

func validateCountdown(c *apiContent) error {
	if c.EndDate == nil {
		return fmt.Errorf("end_date is required for countdown template")
	}
	if *c.EndDate <= 0 {
		return fmt.Errorf("end_date must be > 0")
	}
	if c.StartDate != nil {
		if *c.StartDate <= 0 {
			return fmt.Errorf("start_date must be > 0")
		}
		if *c.StartDate >= *c.EndDate {
			return fmt.Errorf("start_date must be < end_date")
		}
	}
	if c.WarningThreshold != nil && *c.WarningThreshold < 0 {
		return fmt.Errorf("warning_threshold must be >= 0")
	}
	return nil
}

func validateGauge(c *apiContent) error {
	if c.Value == nil {
		return fmt.Errorf("value is required for gauge template")
	}
	v, ok := toFloat64(c.Value)
	if !ok {
		return fmt.Errorf("gauge value must be a number")
	}
	if c.MinValue == nil {
		return fmt.Errorf("min_value is required for gauge template")
	}
	if c.MaxValue == nil {
		return fmt.Errorf("max_value is required for gauge template")
	}
	if *c.MinValue >= *c.MaxValue {
		return fmt.Errorf("min_value must be < max_value")
	}
	if v < *c.MinValue || v > *c.MaxValue {
		return fmt.Errorf("value must be >= min_value and <= max_value")
	}
	if utf8.RuneCountInString(c.Unit) > 32 {
		return fmt.Errorf("unit must be at most 32 runes")
	}
	return nil
}

func validateTimeline(c *apiContent) error {
	if c.Value == nil {
		return fmt.Errorf("value is required for timeline template")
	}
	values := toStringFloat64Map(c.Value)
	if values == nil {
		return fmt.Errorf("timeline value must be a labeled map (e.g. {\"CPU\": 72.5})")
	}
	if c.Scale != "" {
		switch c.Scale {
		case "linear", "logarithmic":
		default:
			return fmt.Errorf("scale must be \"linear\" or \"logarithmic\", got %q", c.Scale)
		}
	}
	if c.Decimals != nil && (*c.Decimals < 0 || *c.Decimals > 10) {
		return fmt.Errorf("decimals must be between 0 and 10, got %d", *c.Decimals)
	}
	if utf8.RuneCountInString(c.Unit) > 32 {
		return fmt.Errorf("unit must be at most 32 runes")
	}
	if len(c.Thresholds) > 5 {
		return fmt.Errorf("thresholds must have at most 5 entries, got %d", len(c.Thresholds))
	}
	for i, th := range c.Thresholds {
		if err := validateColor(th.Color, fmt.Sprintf("thresholds[%d].color", i)); err != nil {
			return err
		}
		if utf8.RuneCountInString(th.Label) > 12 {
			return fmt.Errorf("thresholds[%d].label must be at most 12 runes", i)
		}
	}
	if len(values) > 4 {
		return fmt.Errorf("value must have at most 4 series, got %d", len(values))
	}
	for k := range values {
		if utf8.RuneCountInString(k) > 32 {
			return fmt.Errorf("value key %q must be at most 32 characters", k)
		}
	}
	return nil
}

func validateURL(u, field string) error {
	if u == "" {
		return nil
	}
	if utf8.RuneCountInString(u) > 2048 {
		return fmt.Errorf("%s must be at most 2048 runes", field)
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("%s must start with http:// or https://", field)
	}
	return nil
}

func validateColor(c, field string) error {
	if c == "" {
		return nil
	}
	if namedColors[c] {
		return nil
	}
	if hexColor.MatchString(c) {
		return nil
	}
	return fmt.Errorf("%s must be a named color or hex (#RRGGBB or #RRGGBBAA)", field)
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func toStringFloat64Map(v any) map[string]float64 {
	switch m := v.(type) {
	case map[string]float64:
		return m
	case map[string]any:
		result := make(map[string]float64, len(m))
		for k, val := range m {
			f, ok := toFloat64(val)
			if !ok {
				return nil
			}
			result[k] = f
		}
		return result
	default:
		return nil
	}
}

// RequireValueMap extracts a map[string]float64 from a polymorphic value field,
// failing the test if the value is nil or not a map.
func RequireValueMap(t testing.TB, v any) map[string]float64 {
	t.Helper()
	m := toStringFloat64Map(v)
	if m == nil {
		t.Fatalf("expected map[string]float64, got %T", v)
	}
	return m
}
