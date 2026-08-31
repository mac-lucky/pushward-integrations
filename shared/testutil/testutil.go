package testutil

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// APICall records a PushWard API call made by a handler/poller under test.
type APICall struct {
	Method string
	Path   string
	Body   json.RawMessage
}

var (
	slugPattern    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)
	hexColor       = regexp.MustCompile(`^#[0-9a-fA-F]{6}([0-9a-fA-F]{2})?$`)
	validTemplates = map[string]bool{"generic": true, "alert": true, "steps": true, "countdown": true, "gauge": true, "timeline": true, "board": true, "log": true, "media": true, "approval": true}
	// validTrends / validLogLevels include "" because trend and level are
	// optional - an omitted value is valid; only a non-empty unknown value fails.
	validTrends     = map[string]bool{"": true, pushward.TrendUp: true, pushward.TrendDown: true, pushward.TrendFlat: true}
	validLogLevels  = map[string]bool{"": true, pushward.LogInfo: true, pushward.LogWarn: true, pushward.LogError: true}
	validTapMethods = map[string]bool{"": true, "GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true}
	validStates     = map[string]bool{pushward.StateOngoing: true, pushward.StateEnded: true}
	validSeverities = map[string]bool{"critical": true, "warning": true, "info": true}
	// validImageShapes includes "" because image_shape is optional; the server
	// stores an omitted shape verbatim and the client renders it as square.
	validImageShapes = map[string]bool{
		"":                                true,
		string(pushward.ImageShapePoster): true,
		string(pushward.ImageShapeSquare): true,
		string(pushward.ImageShapeCircle): true,
	}
	// imageTemplates are the only three templates the server accepts an
	// activity image on; anything else is a 422.
	imageTemplates = map[string]bool{pushward.TemplateGeneric: true, pushward.TemplateSteps: true, pushward.TemplateMedia: true}
	// validPlaybackStates includes "" because playback_state is optional; the
	// server defaults an omitted value to paused.
	validPlaybackStates = map[string]bool{
		"":                                 true,
		string(pushward.PlaybackPlaying):   true,
		string(pushward.PlaybackPaused):    true,
		string(pushward.PlaybackStopped):   true,
		string(pushward.PlaybackBuffering): true,
	}
	// approvalOptionID is the server's option-id charset: the slug charset at
	// half the length.
	approvalOptionID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
	// validApprovalStyles includes "" because style is optional; with it
	// omitted the first option renders primary and the rest secondary.
	validApprovalStyles = map[string]bool{
		"":                                      true,
		string(pushward.ApprovalStylePrimary):   true,
		string(pushward.ApprovalStyleSecondary): true,
		string(pushward.ApprovalStyleDestructive): true,
	}
)

// Media template bounds, mirroring the server's validation. The clock-skew
// allowance is the same one the server applies to fired_at and history points.
const (
	maxMediaTitleRunes      = 128
	maxMediaDurationSeconds = 604800 // 7 days
	maxMediaExtraControls   = 3
	maxMediaClockSkew       = 300 * time.Second

	// maxTTLSeconds is the server's shared ceiling for ended_ttl and stale_ttl
	// (30 days). dismissal_ttl has its own, much lower one.
	maxTTLSeconds = 2592000
)

type createRequest struct {
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	Priority     *int   `json:"priority,omitempty"`
	EndedTTL     *int   `json:"ended_ttl,omitempty"`
	StaleTTL     *int   `json:"stale_ttl,omitempty"`
	DismissalTTL *int   `json:"dismissal_ttl,omitempty"`
}

type updateRequest struct {
	State        string     `json:"state"`
	Content      apiContent `json:"content"`
	Priority     *int       `json:"priority,omitempty"`
	EndedTTL     *int       `json:"ended_ttl,omitempty"`
	StaleTTL     *int       `json:"stale_ttl,omitempty"`
	DismissalTTL *int       `json:"dismissal_ttl,omitempty"`
}

type apiContent struct {
	Template           string               `json:"template"`
	Progress           float64              `json:"progress"`
	State              string               `json:"state,omitempty"`
	Icon               string               `json:"icon,omitempty"`
	Subtitle           string               `json:"subtitle,omitempty"`
	AccentColor        string               `json:"accent_color,omitempty"`
	BackgroundColor    string               `json:"background_color,omitempty"`
	TextColor          string               `json:"text_color,omitempty"`
	CurrentStep        *int                 `json:"current_step,omitempty"`
	TotalSteps         *int                 `json:"total_steps,omitempty"`
	StepRows           []int                `json:"step_rows,omitempty"`
	StepLabels         []string             `json:"step_labels,omitempty"`
	StepColors         []string             `json:"step_colors,omitempty"`
	StepWeights        []float64            `json:"step_weights,omitempty"`
	URL                string               `json:"url,omitempty"`
	SecondaryURL       string               `json:"secondary_url,omitempty"`
	Severity           string               `json:"severity,omitempty"`
	FiredAt            *int64               `json:"fired_at,omitempty"`
	SeverityLabel      string               `json:"severity_label,omitempty"`
	RemainingTime      *int                 `json:"remaining_time,omitempty"`
	CompletionMessage  string               `json:"completion_message,omitempty"`
	EndDate            *int64               `json:"end_date,omitempty"`
	StartDate          *int64               `json:"start_date,omitempty"`
	WarningThreshold   *int                 `json:"warning_threshold,omitempty"`
	Value              any                  `json:"value,omitempty"`
	MinValue           *float64             `json:"min_value,omitempty"`
	MaxValue           *float64             `json:"max_value,omitempty"`
	Unit               string               `json:"unit,omitempty"`
	Scale              string               `json:"scale,omitempty"`
	Decimals           *int                 `json:"decimals,omitempty"`
	Smoothing          *bool                `json:"smoothing,omitempty"`
	Thresholds         []testThreshold      `json:"thresholds,omitempty"`
	Duration           *string              `json:"duration,omitempty"`
	Tiles              []testBoardTile      `json:"tiles,omitempty"`
	Lines              []testLogLine        `json:"lines,omitempty"`
	TapAction          *testTapAction       `json:"tap_action,omitempty"`
	URLAction          *testTapAction       `json:"url_action,omitempty"`
	SecondaryURLAction *testTapAction       `json:"secondary_url_action,omitempty"`
	ImageURL           string               `json:"image_url,omitempty"`
	ImageShape         string               `json:"image_shape,omitempty"`
	ImageThumbhash     string               `json:"image_thumbhash,omitempty"`
	MediaTitle         string               `json:"media_title,omitempty"`
	PlaybackState      string               `json:"playback_state,omitempty"`
	PositionSeconds    *float64             `json:"position_seconds,omitempty"`
	DurationSeconds    *float64             `json:"duration_seconds,omitempty"`
	PositionAt         *int64               `json:"position_at,omitempty"`
	Volume             *float64             `json:"volume,omitempty"`
	Favorite           *bool                `json:"favorite,omitempty"`
	Controls           *testMediaControls   `json:"controls,omitempty"`
	Options            []testApprovalOption `json:"options,omitempty"`
	Source             string               `json:"source,omitempty"`
	Details            []testApprovalDetail `json:"details,omitempty"`
	OnExpire           string               `json:"on_expire,omitempty"`
	Answer             map[string]any       `json:"answer,omitempty"`
}

// testApprovalOption mirrors pushward.ApprovalOption.
type testApprovalOption struct {
	ID         string            `json:"id"`
	Title      string            `json:"title"`
	Style      string            `json:"style,omitempty"`
	Icon       string            `json:"icon,omitempty"`
	URL        string            `json:"url,omitempty"`
	Foreground bool              `json:"foreground,omitempty"`
	Method     string            `json:"method,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
}

// testApprovalDetail mirrors pushward.ApprovalDetail.
type testApprovalDetail struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type testThreshold struct {
	Value float64 `json:"value"`
	Color string  `json:"color,omitempty"`
	Label string  `json:"label,omitempty"`
}

type testBoardTile struct {
	Label     string         `json:"label"`
	Value     string         `json:"value"`
	Unit      string         `json:"unit,omitempty"`
	Icon      string         `json:"icon,omitempty"`
	Color     string         `json:"color,omitempty"`
	Trend     string         `json:"trend,omitempty"`
	URLAction *testTapAction `json:"url_action,omitempty"`
}

type testLogLine struct {
	Text  string `json:"text"`
	At    *int64 `json:"at,omitempty"`
	Level string `json:"level,omitempty"`
}

type testTapAction struct {
	URL        string            `json:"url"`
	Foreground bool              `json:"foreground,omitempty"`
	Method     string            `json:"method,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	Title      string            `json:"title,omitempty"`
	Icon       string            `json:"icon,omitempty"`
}

// testMediaControls mirrors pushward.MediaControls: nine named slots plus up
// to three extras.
type testMediaControls struct {
	Previous   *testTapAction  `json:"previous,omitempty"`
	PlayPause  *testTapAction  `json:"play_pause,omitempty"`
	Play       *testTapAction  `json:"play,omitempty"`
	Pause      *testTapAction  `json:"pause,omitempty"`
	Next       *testTapAction  `json:"next,omitempty"`
	Stop       *testTapAction  `json:"stop,omitempty"`
	Favorite   *testTapAction  `json:"favorite,omitempty"`
	VolumeDown *testTapAction  `json:"volume_down,omitempty"`
	VolumeUp   *testTapAction  `json:"volume_up,omitempty"`
	Extra      []testTapAction `json:"extra,omitempty"`
}

// testNotificationAction mirrors pushward.NotificationAction. Unlike a tap
// action its URL is optional (an action may exist purely to surface its id to
// the app), so it gets its own validator rather than reusing validateTapAction.
type testNotificationAction struct {
	ID      string            `json:"id"`
	Title   string            `json:"title"`
	URL     string            `json:"url,omitempty"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Icon    string            `json:"icon,omitempty"`
}

// MockPushWardServer starts an httptest server that records all requests and
// validates them against the PushWard public API contract.
func MockPushWardServer(t *testing.T) (*httptest.Server, *[]APICall, *sync.Mutex) {
	t.Helper()
	return mockPushWardServer(t, 0, 0)
}

// MockPushWardServerRejecting is MockPushWardServer answering notifyStatus to
// every POST /notifications and activityStatus to every POST /activities. Pass
// 0 for either to keep that side on the success path, which is what separates
// the two cases worth testing: a push the server refuses while activities still
// work, and an integration key it refuses outright (401/403) or a tenant over
// quota (429), where nothing gets through. Handlers are expected to surface the
// latter unchanged rather than as a generic 502, since the caller is the one
// holding the key. Requests are validated before being rejected, so a test can
// tell a rejected call from a malformed one.
func MockPushWardServerRejecting(t *testing.T, notifyStatus, activityStatus int) (*httptest.Server, *[]APICall, *sync.Mutex) {
	t.Helper()
	return mockPushWardServer(t, notifyStatus, activityStatus)
}

// AssertUpstreamRefusalSurfaces checks that a webhook handler passes an upstream
// refusal of the caller's own key back to the caller unchanged, rather than
// collapsing it into a generic 502. The relay forwards the caller's hlk_ key
// rather than one of its own, so "the next hop rejected this key" is a fact
// about the sender's configuration, and answering Bad Gateway sends them looking
// for an outage instead.
//
// deliver builds a handler whose upstream answers status to everything and puts
// one webhook through it, returning the recorded response.
//
// 429 is deliberately not covered: the pushward client retries it five times
// with backoff, so driving it through a handler costs ~11s of sleeping to prove
// what humautil's own TestUpstreamError already covers directly.
func AssertUpstreamRefusalSurfaces(t *testing.T, deliver func(t *testing.T, status int) *httptest.ResponseRecorder) {
	t.Helper()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			w := deliver(t, status)
			if w.Code != status {
				t.Fatalf("expected %d, got %d (%s)", status, w.Code, w.Body.String())
			}
			if status == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") == "" {
				t.Error("RFC 9110 section 15.5.2 requires a challenge on a 401")
			}
		})
	}
}

func mockPushWardServer(t *testing.T, notifyStatus, activityStatus int) (*httptest.Server, *[]APICall, *sync.Mutex) {
	t.Helper()
	if notifyStatus == 0 {
		notifyStatus = http.StatusCreated
	}
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

		if activityStatus >= 400 {
			respondError(w, activityStatus, "activity rejected")
			return
		}

		// POST /activities is an upsert - always 201, never 409 for duplicate
		// slug. X-Resource-Action distinguishes the two cases.
		mu.Lock()
		action := "created"
		if slugs[req.Slug] {
			action = "updated"
		}
		slugs[req.Slug] = true
		mu.Unlock()
		w.Header().Set("X-Resource-Action", action)
		w.WriteHeader(http.StatusCreated)
	})

	mux.HandleFunc("PATCH /activities/", func(w http.ResponseWriter, r *http.Request) {
		body := recordCall(&calls, &mu, r)

		slug := strings.TrimPrefix(r.URL.Path, "/activities/")
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
			Title        string                   `json:"title"`
			Body         string                   `json:"body"`
			ActivitySlug string                   `json:"activity_slug,omitempty"`
			Actions      []testNotificationAction `json:"actions,omitempty"`
			Push         *bool                    `json:"push,omitempty"`
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
		if req.ActivitySlug != "" && !slugPattern.MatchString(req.ActivitySlug) {
			respondError(w, http.StatusBadRequest, "invalid activity_slug: "+req.ActivitySlug)
			return
		}
		if len(req.Actions) > 10 {
			respondError(w, http.StatusBadRequest, "actions must have at most 10 entries")
			return
		}
		for i, a := range req.Actions {
			if err := validateNotificationAction(a, fmt.Sprintf("actions[%d]", i)); err != nil {
				respondError(w, http.StatusBadRequest, err.Error())
				return
			}
		}

		if notifyStatus >= 400 {
			respondError(w, notifyStatus, "notification rejected")
			return
		}

		pushed := req.Push == nil || *req.Push

		w.WriteHeader(notifyStatus)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "pushed": pushed})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		recordCall(&calls, &mu, r)
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls, &mu
}

// MockPushWardServerFailingPatches records every PushWard call and fails the
// first failPatches PATCH /activities/ requests with 400 (which the pushward
// client treats as fail-fast, keeping the test quick), succeeding on subsequent
// PATCHes. POST /activities and POST /notifications always succeed. It drives
// handler update-failure scenarios where a final ONGOING tick fails but the
// lifecycle must still proceed.
func MockPushWardServerFailingPatches(t *testing.T, failPatches int) (*httptest.Server, *[]APICall, *sync.Mutex) {
	t.Helper()
	var calls []APICall
	var mu sync.Mutex
	patchN := 0

	mux := http.NewServeMux()
	mux.HandleFunc("POST /activities", func(w http.ResponseWriter, r *http.Request) {
		recordCall(&calls, &mu, r)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST /notifications", func(w http.ResponseWriter, r *http.Request) {
		recordCall(&calls, &mu, r)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("PATCH /activities/", func(w http.ResponseWriter, r *http.Request) {
		recordCall(&calls, &mu, r)
		mu.Lock()
		patchN++
		fail := patchN <= failPatches
		mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls, &mu
}

// CountPath returns the number of recorded calls whose path equals path.
func CountPath(calls []APICall, path string) int {
	n := 0
	for _, c := range calls {
		if c.Path == path {
			n++
		}
	}
	return n
}

// GetCalls returns a snapshot of the recorded API calls.
func GetCalls(calls *[]APICall, mu *sync.Mutex) []APICall {
	mu.Lock()
	defer mu.Unlock()
	result := make([]APICall, len(*calls))
	copy(result, *calls)
	return result
}

// WaitForCalls polls until at least n calls have been recorded, then settles
// briefly and returns everything recorded by the end of that window. A loaded CI
// box gets more time instead of a flake, and a caller asserting an exact count
// still sees a call that arrives just after the nth.
func WaitForCalls(t *testing.T, calls *[]APICall, mu *sync.Mutex, n int, timeout time.Duration) []APICall {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := GetCalls(calls, mu)
		if len(got) >= n {
			time.Sleep(10 * time.Millisecond)
			return GetCalls(calls, mu)
		}
		if time.Now().After(deadline) {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// UnmarshalBody decodes the JSON body of a recorded API call into v.
func UnmarshalBody(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("failed to unmarshal body: %v (body: %s)", err, string(raw))
	}
}

// Thumbhash is a real ThumbHash (of a 32x48 gradient), so a payload carrying it
// survives the base64 validation MockPushWardServer applies to image_thumbhash.
// Pair it with poster.Static to give a handler artwork without a network fetch.
const Thumbhash = "WfcJhRqPdTeXeIhXiXiYd3BmB/eH"

// LastActivityUpdate returns the content of the last PATCH /activities call,
// which is the frame that actually reaches the Lock Screen.
func LastActivityUpdate(t *testing.T, calls []APICall) pushward.Content {
	t.Helper()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Method == "PATCH" {
			var update pushward.UpdateRequest
			UnmarshalBody(t, calls[i].Body, &update)
			return update.Content
		}
	}
	t.Fatal("no PATCH /activities call recorded")
	return pushward.Content{}
}

// AssertImageTrio checks the three activity image fields together: a shape
// without a URL or a hash renders nothing, so they are only ever meaningful as
// a set.
func AssertImageTrio(t *testing.T, c pushward.Content, wantURL, wantHash string, wantShape pushward.ImageShape) {
	t.Helper()
	if c.ImageURL != wantURL {
		t.Errorf("image_url = %q, want %q", c.ImageURL, wantURL)
	}
	if c.ImageThumbhash != wantHash {
		t.Errorf("image_thumbhash = %q, want %q", c.ImageThumbhash, wantHash)
	}
	if c.ImageShape != wantShape {
		t.Errorf("image_shape = %q, want %q", c.ImageShape, wantShape)
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
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "about:blank",
		"title":  http.StatusText(code),
		"status": code,
		"detail": msg,
	})
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
	if err := validateTTLs(req.EndedTTL, req.StaleTTL); err != nil {
		return err
	}
	// dismissal_ttl allows 0 where the other two do not: it is the "remove the
	// card immediately" value, and the whole reason the field is a pointer.
	return validateDismissalTTL(req.DismissalTTL)
}

// validateTTLs range-checks the ended/stale pair, which the create and update
// paths bound identically.
func validateTTLs(endedTTL, staleTTL *int) error {
	if endedTTL != nil && (*endedTTL < 1 || *endedTTL > maxTTLSeconds) {
		return fmt.Errorf("ended_ttl must be 1-%d", maxTTLSeconds)
	}
	if staleTTL != nil && (*staleTTL < 1 || *staleTTL > maxTTLSeconds) {
		return fmt.Errorf("stale_ttl must be 1-%d", maxTTLSeconds)
	}
	return nil
}

func validateDismissalTTL(v *int) error {
	if v != nil && (*v < 0 || *v > pushward.DismissalTTLMax) {
		return fmt.Errorf("dismissal_ttl must be 0-%d", pushward.DismissalTTLMax)
	}
	return nil
}

func validateUpdateRequest(req *updateRequest) error {
	// state and content.template are optional under RFC 7396 merge-patch -
	// an absent field means "preserve server-side value". Only validate when
	// present (non-empty).
	if req.State != "" && !validStates[req.State] {
		return fmt.Errorf("state must be ongoing or ended")
	}
	if req.Priority != nil && (*req.Priority < 0 || *req.Priority > 10) {
		return fmt.Errorf("priority must be 0-10")
	}
	if err := validateTTLs(req.EndedTTL, req.StaleTTL); err != nil {
		return err
	}
	if err := validateDismissalTTL(req.DismissalTTL); err != nil {
		return err
	}
	return validateContent(&req.Content)
}

func validateContent(c *apiContent) error {
	// Under merge-patch, template may be absent on ticks; only per-template
	// required-field validation is gated on it.
	if c.Template != "" && !validTemplates[c.Template] {
		return fmt.Errorf("template must be one of: generic, alert, steps, countdown, gauge, timeline, board, log, media, approval")
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
	// url / secondary_url (and the structured tap-action slots) are accepted on
	// every template - the server no longer gates tap routing to steps/alert.
	if err := validateColor(c.AccentColor, "accent_color"); err != nil {
		return err
	}
	if err := validateColor(c.BackgroundColor, "background_color"); err != nil {
		return err
	}
	if err := validateColor(c.TextColor, "text_color"); err != nil {
		return err
	}
	if err := validateTapAction(c.TapAction, "tap_action"); err != nil {
		return err
	}
	if err := validateTapAction(c.URLAction, "url_action"); err != nil {
		return err
	}
	if err := validateTapAction(c.SecondaryURLAction, "secondary_url_action"); err != nil {
		return err
	}
	if err := validateImageFields(c); err != nil {
		return err
	}
	if err := validateMedia(c); err != nil {
		return err
	}
	if err := validateApprovalFields(c); err != nil {
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
	case "board":
		if err := validateBoard(c); err != nil {
			return err
		}
	case "log":
		if err := validateLog(c); err != nil {
			return err
		}
	case "approval":
		if err := validateApproval(c); err != nil {
			return err
		}
	}

	return nil
}

// validateApprovalFields is the off-template gate: approval fields on any
// other template are a 422, same shape as validateMedia's gate. Answer counts
// here (the server's hasApprovalFields includes it) even though on the
// approval template itself a client-sent answer is stripped, not rejected.
func validateApprovalFields(c *apiContent) error {
	hasApproval := len(c.Options) > 0 || c.Source != "" || len(c.Details) > 0 ||
		c.OnExpire != "" || c.Answer != nil
	if hasApproval && c.Template != "" && c.Template != pushward.TemplateApproval {
		return fmt.Errorf("options, source, details, on_expire and answer are only valid on the approval template, got %q", c.Template)
	}
	return nil
}

// validateApproval re-implements the server's approval template rules: 2-4
// options with unique slug-charset ids, the two generic button slots
// reserved, and on_expire tied to end_date and an option id. The per-option
// rules live in validateApprovalOption. The server also fills an empty method
// with POST on http(s) options (not echoed here; the mock is stateless) and
// strips a client-sent answer instead of rejecting it.
func validateApproval(c *apiContent) error {
	if len(c.Options) < 2 || len(c.Options) > 4 {
		return fmt.Errorf("options must have 2-4 entries for approval template, got %d", len(c.Options))
	}
	if c.URLAction != nil || c.SecondaryURLAction != nil {
		return fmt.Errorf("url_action and secondary_url_action are not valid on the approval template")
	}
	// The server also rejects alarm / snooze_seconds on approval; the mock's
	// apiContent does not model those countdown fields, so that rule is left
	// to the real server.
	seen := make(map[string]bool, len(c.Options))
	for i := range c.Options {
		field := fmt.Sprintf("options[%d]", i)
		o := &c.Options[i]
		if err := validateApprovalOption(o, field, len(c.Options)); err != nil {
			return err
		}
		if seen[o.ID] {
			return fmt.Errorf("%s.id %q is already used", field, o.ID)
		}
		seen[o.ID] = true
	}
	if utf8.RuneCountInString(c.Source) > 24 {
		return fmt.Errorf("source must be at most 24 runes")
	}
	if len(c.Details) > 2 {
		return fmt.Errorf("details must have at most 2 entries")
	}
	for i, d := range c.Details {
		if d.Label == "" || utf8.RuneCountInString(d.Label) > 24 {
			return fmt.Errorf("details[%d].label is required and at most 24 runes", i)
		}
		if d.Value == "" || utf8.RuneCountInString(d.Value) > 64 {
			return fmt.Errorf("details[%d].value is required and at most 64 runes", i)
		}
	}
	if c.OnExpire == "" {
		return nil
	}
	if c.EndDate == nil {
		return fmt.Errorf("on_expire requires end_date")
	}
	if c.OnExpire != pushward.ApprovalAnswerNone && !seen[c.OnExpire] {
		return fmt.Errorf("on_expire must be \"none\" or an option id, got %q", c.OnExpire)
	}
	return nil
}

// validateApprovalOption checks one answer button: its identity (slug-shaped
// id, short title, known style, and an icon once total reaches three, where
// the buttons render as icon-first tiles), then its routing.
//
// Mode A is the producer's own endpoint, validated like any other action and,
// on http(s), always silent. Mode B omits url entirely and leaves the slot
// for the signed answer URL the server fills in: that fill overwrites method
// with POST and passes headers and body through untouched, so none of the
// three is rejected here. Foreground still is - the filled URL is http(s),
// where the server refuses the foreground shape (an explicit false marshals
// away and reads as absent).
func validateApprovalOption(o *testApprovalOption, field string, total int) error {
	if !approvalOptionID.MatchString(o.ID) {
		return fmt.Errorf("%s.id is required and must be slug-shaped (at most 64 chars)", field)
	}
	if o.Title == "" || utf8.RuneCountInString(o.Title) > 24 {
		return fmt.Errorf("%s.title is required and at most 24 runes", field)
	}
	if !validApprovalStyles[o.Style] {
		return fmt.Errorf("%s.style must be primary, secondary or destructive", field)
	}
	if utf8.RuneCountInString(o.Icon) > 64 {
		return fmt.Errorf("%s.icon must be at most 64 runes", field)
	}
	if o.Icon == "" && total >= 3 {
		return fmt.Errorf("%s.icon is required with three or more options", field)
	}
	if o.URL == "" {
		if o.Foreground {
			return fmt.Errorf("%s.foreground must not be true without a url: the server fills an http(s) answer url and approval options are silent webhooks", field)
		}
		// The fill makes the option http(s), so the body cap still applies.
		if utf8.RuneCountInString(o.Body) > 1024 {
			return fmt.Errorf("%s.body must be at most 1024 characters", field)
		}
		return nil
	}
	action := testTapAction{URL: o.URL, Foreground: o.Foreground, Method: o.Method, Headers: o.Headers, Body: o.Body}
	return validateSilentAction(&action, field, "approval options")
}

func validateAlert(c *apiContent) error {
	if !validSeverities[c.Severity] {
		return fmt.Errorf("severity is required and must be critical, warning, or info")
	}
	if utf8.RuneCountInString(c.SeverityLabel) > pushward.MaxSeverityLabelRunes {
		return fmt.Errorf("severity_label must be at most %d runes", pushward.MaxSeverityLabelRunes)
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
			if v < pushward.MinStepRows || v > pushward.MaxStepRows {
				return fmt.Errorf("step_rows[%d] must be %d-%d", i, pushward.MinStepRows, pushward.MaxStepRows)
			}
		}
	}
	if c.StepLabels != nil {
		if len(c.StepLabels) != *c.TotalSteps {
			return fmt.Errorf("step_labels length must equal total_steps")
		}
		for i, label := range c.StepLabels {
			if utf8.RuneCountInString(label) > pushward.MaxStepLabelLen {
				return fmt.Errorf("step_labels[%d] must be at most %d runes", i, pushward.MaxStepLabelLen)
			}
		}
	}
	if c.StepColors != nil && len(c.StepColors) != *c.TotalSteps {
		return fmt.Errorf("step_colors length must equal total_steps")
	}
	if c.StepWeights != nil {
		if len(c.StepWeights) != *c.TotalSteps {
			return fmt.Errorf("step_weights length must equal total_steps")
		}
		for i, w := range c.StepWeights {
			if w < 0 {
				return fmt.Errorf("step_weights[%d] must not be negative", i)
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

func validateBoard(c *apiContent) error {
	if len(c.Tiles) < 1 || len(c.Tiles) > 4 {
		return fmt.Errorf("tiles must have between 1 and 4 entries for board template, got %d", len(c.Tiles))
	}
	for i, tile := range c.Tiles {
		if tile.Label == "" {
			return fmt.Errorf("tiles[%d].label is required", i)
		}
		if utf8.RuneCountInString(tile.Label) > 32 {
			return fmt.Errorf("tiles[%d].label must be at most 32 characters", i)
		}
		if tile.Value == "" {
			return fmt.Errorf("tiles[%d].value is required", i)
		}
		if utf8.RuneCountInString(tile.Value) > 16 {
			return fmt.Errorf("tiles[%d].value must be at most 16 characters", i)
		}
		if utf8.RuneCountInString(tile.Unit) > 8 {
			return fmt.Errorf("tiles[%d].unit must be at most 8 characters", i)
		}
		if utf8.RuneCountInString(tile.Icon) > 128 {
			return fmt.Errorf("tiles[%d].icon must be at most 128 characters", i)
		}
		if err := validateColor(tile.Color, fmt.Sprintf("tiles[%d].color", i)); err != nil {
			return err
		}
		if !validTrends[tile.Trend] {
			return fmt.Errorf("tiles[%d].trend must be one of up, down, flat", i)
		}
		if err := validateTapAction(tile.URLAction, fmt.Sprintf("tiles[%d].url_action", i)); err != nil {
			return err
		}
	}
	return nil
}

// validateTapAction mirrors the server's TapAction.Validate for the checks the
// mock cares about, adding the url-is-required rule on top of the shared action
// shape. It does not reproduce the server's full http-only cross-field matrix:
// the mock is a contract sanity check, not a reimplementation. Custom schemes
// (e.g. homeassistant://) are allowed.
func validateTapAction(a *testTapAction, field string) error {
	if a == nil {
		return nil
	}
	if a.URL == "" {
		return fmt.Errorf("%s.url is required", field)
	}
	return validateActionShape(actionShape{URL: a.URL, Method: a.Method, Title: a.Title, Icon: a.Icon, Body: a.Body}, field)
}

// validateNotificationAction mirrors the server's NotificationAction validation
// for the checks the mock cares about. As with validateTapAction this is a
// contract sanity check, not a reimplementation of the server's cross-field
// matrix. Unlike a tap action, url is optional: an action may exist only to
// surface its id to the app.
func validateNotificationAction(a testNotificationAction, field string) error {
	if a.ID == "" {
		return fmt.Errorf("%s.id is required", field)
	}
	if a.Title == "" {
		return fmt.Errorf("%s.title is required", field)
	}
	if utf8.RuneCountInString(a.ID) > 64 {
		return fmt.Errorf("%s.id must be at most 64 characters", field)
	}
	return validateActionShape(actionShape{URL: a.URL, Method: a.Method, Title: a.Title, Icon: a.Icon, Body: a.Body}, field)
}

// actionShape is the field set that tap actions and notification actions
// validate identically. It mirrors the server's ActionFields/ValidateAction
// split, where the shared URL/method/caps rules live in one place and each
// action type layers on its own required-field rules.
type actionShape struct {
	URL, Method, Title, Icon, Body string
}

func validateActionShape(a actionShape, field string) error {
	if a.URL != "" && blockedScheme(a.URL) {
		return fmt.Errorf("%s.url scheme is not allowed", field)
	}
	if !validTapMethods[strings.ToUpper(a.Method)] {
		return fmt.Errorf("%s.method must be one of GET, POST, PUT, PATCH, DELETE, HEAD", field)
	}
	if utf8.RuneCountInString(a.Title) > 64 {
		return fmt.Errorf("%s.title must be at most 64 characters", field)
	}
	if utf8.RuneCountInString(a.Icon) > 64 {
		return fmt.Errorf("%s.icon must be at most 64 characters", field)
	}
	if utf8.RuneCountInString(a.Body) > 1024 {
		return fmt.Errorf("%s.body must be at most 1024 characters", field)
	}
	return nil
}

// blockedScheme reports whether a URL uses a scheme the server rejects outright
// on any action object.
func blockedScheme(rawURL string) bool {
	lower := strings.ToLower(rawURL)
	for _, bad := range []string{"javascript:", "data:", "file:", "vbscript:"} {
		if strings.HasPrefix(lower, bad) {
			return true
		}
	}
	return false
}

func validateLog(c *apiContent) error {
	if len(c.Lines) < 1 || len(c.Lines) > 20 {
		return fmt.Errorf("lines must have between 1 and 20 entries for log template, got %d", len(c.Lines))
	}
	for i, line := range c.Lines {
		if line.Text == "" {
			return fmt.Errorf("lines[%d].text is required", i)
		}
		if utf8.RuneCountInString(line.Text) > 512 {
			return fmt.Errorf("lines[%d].text must be at most 512 characters", i)
		}
		if !validLogLevels[line.Level] {
			return fmt.Errorf("lines[%d].level must be one of info, warn, error", i)
		}
	}
	return nil
}

// validateImageFields mirrors the server's activity-image rules. It is
// deliberately stricter than validateURL and does not reuse it: an activity
// image is https-only with a host and no userinfo, while url / secondary_url
// still accept plain http.
//
// The template gate only fires when the template is present. Under merge-patch
// a tick may omit it, and the mock is stateless - it cannot know the stored
// template - so a template-less patch carrying image fields passes here and is
// left to the real server.
func validateImageFields(c *apiContent) error {
	hasImage := c.ImageURL != "" || c.ImageShape != "" || c.ImageThumbhash != ""
	if hasImage && c.Template != "" && !imageTemplates[c.Template] {
		return fmt.Errorf("image_url, image_shape and image_thumbhash are only valid on the generic, steps and media templates, got %q", c.Template)
	}
	if c.ImageURL != "" {
		if utf8.RuneCountInString(c.ImageURL) > 2048 {
			return fmt.Errorf("image_url must be at most 2048 runes")
		}
		u, err := url.Parse(c.ImageURL)
		if err != nil {
			return fmt.Errorf("image_url is not a valid URL: %w", err)
		}
		if u.Scheme != "https" {
			return fmt.Errorf("image_url must use https")
		}
		if u.Host == "" {
			return fmt.Errorf("image_url must have a host")
		}
		if u.User != nil {
			return fmt.Errorf("image_url must not carry userinfo")
		}
	}
	if !validImageShapes[c.ImageShape] {
		return fmt.Errorf("image_shape must be one of poster, square, circle")
	}
	if c.ImageThumbhash != "" {
		if len(c.ImageThumbhash) > 64 {
			return fmt.Errorf("image_thumbhash must be at most 64 characters")
		}
		if _, err := base64.StdEncoding.DecodeString(c.ImageThumbhash); err != nil {
			return fmt.Errorf("image_thumbhash must be padded standard-alphabet base64: %w", err)
		}
	}
	return nil
}

// validateMedia mirrors the server's media-template rules. Like
// validateImageFields it runs on every payload rather than from the template
// switch: the eight media fields are all optional, so the template case would
// have nothing required to check, while a position tick under merge-patch may
// omit the template and its bounds still matter. The template gate only fires
// when the template is present; a template-less patch carrying media fields is
// left to the real server, since the mock is stateless.
//
// Not reproduced: the server also defaults an empty method to POST on http(s)
// controls (the mock does not echo content back) and rejects fields it does
// not know (JSON here decodes leniently).
func validateMedia(c *apiContent) error {
	hasMedia := c.MediaTitle != "" || c.PlaybackState != "" || c.PositionSeconds != nil ||
		c.DurationSeconds != nil || c.PositionAt != nil || c.Volume != nil || c.Favorite != nil ||
		c.Controls != nil
	if hasMedia && c.Template != "" && c.Template != pushward.TemplateMedia {
		return fmt.Errorf("media_title, playback_state, position_seconds, duration_seconds, position_at, volume, favorite and controls are only valid on the media template, got %q", c.Template)
	}
	if utf8.RuneCountInString(c.MediaTitle) > maxMediaTitleRunes {
		return fmt.Errorf("media_title must be at most %d runes", maxMediaTitleRunes)
	}
	if !validPlaybackStates[c.PlaybackState] {
		return fmt.Errorf("playback_state must be one of playing, paused, stopped, buffering")
	}
	if c.PositionSeconds != nil && *c.PositionSeconds < 0 {
		return fmt.Errorf("position_seconds must be >= 0")
	}
	if c.DurationSeconds != nil && (*c.DurationSeconds <= 0 || *c.DurationSeconds > maxMediaDurationSeconds) {
		return fmt.Errorf("duration_seconds must be > 0 and at most %d", maxMediaDurationSeconds)
	}
	if c.Volume != nil && (*c.Volume < 0 || *c.Volume > 1) {
		return fmt.Errorf("volume must be 0.0-1.0")
	}
	if c.PositionAt != nil {
		if *c.PositionAt <= 0 {
			return fmt.Errorf("position_at must be > 0")
		}
		if *c.PositionAt > time.Now().Add(maxMediaClockSkew).Unix() {
			return fmt.Errorf("position_at must not be more than %s in the future", maxMediaClockSkew)
		}
	}
	return validateMediaControls(c.Controls)
}

// validateMediaControls checks every control slot with the shared tap-action
// rules plus the two media-only ones: an http(s) slot is a silent webhook and
// may not ask for the foreground, and an extra button has no fixed glyph so
// it must bring its own icon.
func validateMediaControls(mc *testMediaControls) error {
	if mc == nil {
		return nil
	}
	slots := []struct {
		name   string
		action *testTapAction
	}{
		{"previous", mc.Previous},
		{"play_pause", mc.PlayPause},
		{"play", mc.Play},
		{"pause", mc.Pause},
		{"next", mc.Next},
		{"stop", mc.Stop},
		{"favorite", mc.Favorite},
		{"volume_down", mc.VolumeDown},
		{"volume_up", mc.VolumeUp},
	}
	for _, s := range slots {
		if err := validateSilentAction(s.action, "controls."+s.name, "media controls"); err != nil {
			return err
		}
	}
	if len(mc.Extra) > maxMediaExtraControls {
		return fmt.Errorf("controls.extra must have at most %d entries, got %d", maxMediaExtraControls, len(mc.Extra))
	}
	for i := range mc.Extra {
		field := fmt.Sprintf("controls.extra[%d]", i)
		if err := validateSilentAction(&mc.Extra[i], field, "media controls"); err != nil {
			return err
		}
		if mc.Extra[i].Icon == "" {
			return fmt.Errorf("%s.icon is required", field)
		}
	}
	return nil
}

// validateSilentAction is the rule the action slots that only ever fire in
// the background share: the tap-action shape, plus the refusal to open the
// foreground from an http(s) URL. kind names the slot family in the error
// ("media controls", "approval options").
func validateSilentAction(a *testTapAction, field, kind string) error {
	if err := validateTapAction(a, field); err != nil {
		return err
	}
	if a != nil && a.Foreground && isHTTPURL(a.URL) {
		return fmt.Errorf("%s.foreground must not be true on an http(s) url: %s are silent webhooks", field, kind)
	}
	return nil
}

func isHTTPURL(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
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
	if hexColor.MatchString(c) {
		return nil
	}
	return fmt.Errorf("%s must be hex (#RRGGBB or #RRGGBBAA)", field)
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
