package argocd

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

const totalSteps = 3

// webhookPayload is the JSON body sent by argocd-notifications webhook templates.
type webhookPayload struct {
	App      string `json:"app"`
	Event    string `json:"event"`
	Revision string `json:"revision"`
	RepoURL  string `json:"repo_url"`
}

// trackedAppState is the JSON-serializable state stored in the state.Store.
type trackedAppState struct {
	Slug     string `json:"slug"`
	Revision string `json:"revision"`
	RepoURL  string `json:"repo_url"`
	Step     int    `json:"step"`
	Pending  bool   `json:"pending"`
}

// timerSet holds in-memory timer references for a tracked app.
type timerSet struct {
	graceTimer *time.Timer
	endTimer   *time.Timer
}

// Handler processes ArgoCD sync webhooks for multiple tenants.
type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.ArgoCDConfig
	mu      sync.Mutex
	timers  map[string]*timerSet // "userKey:appName" → timers
}

// NewHandler creates a new ArgoCD webhook handler.
func NewHandler(store state.Store, clients *client.Pool, cfg *config.ArgoCDConfig) *Handler {
	return &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		timers:  make(map[string]*timerSet),
	}
}

func timerKey(userKey, appName string) string {
	return userKey + ":" + appName
}

func (h *Handler) getTimers(key string) *timerSet {
	ts, ok := h.timers[key]
	if !ok {
		ts = &timerSet{}
		h.timers[key] = ts
	}
	return ts
}

func (h *Handler) saveApp(ctx context.Context, userKey, appName string, app *trackedAppState) error {
	data, err := json.Marshal(app)
	if err != nil {
		return err
	}
	return h.store.Set(ctx, "argocd", userKey, appName, "", data, 1*time.Hour)
}

func (h *Handler) loadApp(ctx context.Context, userKey, appName string) (*trackedAppState, bool, error) {
	data, err := h.store.Get(ctx, "argocd", userKey, appName, "")
	if err != nil {
		return nil, false, err
	}
	if data == nil {
		return nil, false, nil
	}
	var app trackedAppState
	if err := json.Unmarshal(data, &app); err != nil {
		return nil, false, err
	}
	return &app, true, nil
}

func (h *Handler) deleteApp(ctx context.Context, userKey, appName string) {
	_ = h.store.Delete(ctx, "argocd", userKey, appName, "")
}

func (h *Handler) setTombstone(ctx context.Context, userKey, appName string) {
	_ = h.store.Set(ctx, "argocd", userKey, appName, "tombstone", []byte("{}"), h.config.SyncGracePeriod*2)
}

func (h *Handler) hasTombstone(ctx context.Context, userKey, appName string) bool {
	exists, _ := h.store.Exists(ctx, "argocd", userKey, appName, "tombstone")
	return exists
}

// contentURLs returns the url and secondary_url fields for a given app and payload.
func (h *Handler) contentURLs(appName, repoURL, revision string) (string, string) {
	var url, secondaryURL string
	if h.config.URL != "" {
		url = h.config.URL + "/applications/argocd/" + appName
	}
	if repoURL != "" && revision != "" {
		secondaryURL = strings.TrimSuffix(repoURL, ".git") + "/commit/" + revision
	}
	return url, secondaryURL
}

var nonAlphanumeric = regexp.MustCompile(`[^a-z0-9]+`)

// slugForApp derives a stable, URL-safe activity slug from an ArgoCD app name.
func slugForApp(appName string) string {
	s := strings.ToLower(appName)
	s = nonAlphanumeric.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return "argocd-" + s
}

// ServeHTTP handles incoming ArgoCD webhook requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Webhook secret validation
	if h.config.WebhookSecret != "" {
		got := r.Header.Get("X-Webhook-Secret")
		if subtle.ConstantTimeCompare([]byte(got), []byte(h.config.WebhookSecret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var payload webhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		slog.Error("failed to decode webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	if payload.App == "" || payload.Event == "" {
		http.Error(w, "missing app or event", http.StatusBadRequest)
		return
	}

	userKey := auth.KeyFromContext(r.Context())
	ctx := r.Context()

	switch payload.Event {
	case "sync-running":
		h.handleSyncRunning(ctx, userKey, &payload)
	case "sync-succeeded":
		h.handleSyncSucceeded(ctx, userKey, &payload)
	case "deployed":
		h.handleDeployed(ctx, userKey, &payload)
	case "sync-failed":
		h.handleSyncFailed(ctx, userKey, &payload)
	case "health-degraded":
		h.handleHealthDegraded(ctx, userKey, &payload)
	default:
		slog.Warn("unknown event", "event", payload.Event, "app", payload.App)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleSyncRunning(ctx context.Context, userKey string, p *webhookPayload) {
	slug := slugForApp(p.App)
	tk := timerKey(userKey, p.App)

	h.mu.Lock()

	// If a recent-deploy tombstone exists, the sync already completed and this
	// sync-running arrived out of order — skip it entirely.
	if h.hasTombstone(ctx, userKey, p.App) {
		h.mu.Unlock()
		slog.Info("skipped late sync-running (already deployed)", "slug", slug, "app", p.App)
		return
	}

	app, exists, _ := h.loadApp(ctx, userKey, p.App)
	needsCreate := !exists || (p.Revision != "" && app.Revision != p.Revision)

	if needsCreate {
		if exists {
			ts := h.getTimers(tk)
			if ts.graceTimer != nil {
				ts.graceTimer.Stop()
			}
			if ts.endTimer != nil {
				ts.endTimer.Stop()
			}
		}
		app = &trackedAppState{
			Slug:     slug,
			Revision: p.Revision,
			RepoURL:  p.RepoURL,
			Step:     1,
		}
		_ = h.saveApp(ctx, userKey, p.App, app)
	} else {
		app.Step = 1
		_ = h.saveApp(ctx, userKey, p.App, app)
	}

	ts := h.getTimers(tk)
	if ts.endTimer != nil {
		ts.endTimer.Stop()
		ts.endTimer = nil
	}

	// Grace period: defer activity creation for new or still-pending syncs
	gracePeriod := h.config.SyncGracePeriod
	if gracePeriod > 0 && (needsCreate || app.Pending) {
		app.Pending = true
		_ = h.saveApp(ctx, userKey, p.App, app)
		if ts.graceTimer != nil {
			ts.graceTimer.Stop()
		}
		ts.graceTimer = time.AfterFunc(gracePeriod, func() {
			h.graceExpired(userKey, p.App)
		})
		h.mu.Unlock()
		slog.Info("sync started (grace period)", "slug", slug, "app", p.App, "grace", gracePeriod)
		return
	}

	h.mu.Unlock()

	pw := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if needsCreate {
		if err := pw.CreateActivity(ctx, slug, p.App, h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			h.deleteApp(ctx, userKey, p.App)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity", "slug", slug, "app", p.App)
	}

	step := 1
	total := totalSteps
	url, secondaryURL := h.contentURLs(p.App, p.RepoURL, p.Revision)
	req := pushward.UpdateRequest{
		State: "ONGOING",
		Content: pushward.Content{
			Template:     "pipeline",
			Progress:     float64(step) / float64(total),
			State:        "Syncing...",
			Icon:         "arrow.triangle.2.circlepath",
			Subtitle:     "ArgoCD \u00b7 " + p.App,
			AccentColor:  "#007AFF",
			CurrentStep:  &step,
			TotalSteps:   &total,
			URL:          url,
			SecondaryURL: secondaryURL,
		},
	}

	if err := pw.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "step", "1/3", "state", "Syncing...")
}

func (h *Handler) handleSyncSucceeded(ctx context.Context, userKey string, p *webhookPayload) {
	slug := slugForApp(p.App)
	tk := timerKey(userKey, p.App)

	h.mu.Lock()
	app, exists, _ := h.loadApp(ctx, userKey, p.App)

	// Tracked and still in grace period — just advance step, don't touch PushWard
	if exists && app.Pending {
		app.Step = 2
		_ = h.saveApp(ctx, userKey, p.App, app)
		h.mu.Unlock()
		slog.Info("sync succeeded (grace period)", "slug", slug, "app", p.App)
		return
	}

	if !exists {
		// Untracked (bridge restart)
		gracePeriod := h.config.SyncGracePeriod
		if gracePeriod > 0 {
			// If deployed already arrived (out-of-order events), this is a no-op.
			if h.hasTombstone(ctx, userKey, p.App) {
				h.mu.Unlock()
				slog.Info("skipped no-op sync (deployed arrived first)", "slug", slug, "app", p.App)
				return
			}

			// Start grace period at step 2 — if deployed comes quickly, skip
			app = &trackedAppState{
				Slug:     slug,
				Revision: p.Revision,
				RepoURL:  p.RepoURL,
				Step:     2,
				Pending:  true,
			}
			_ = h.saveApp(ctx, userKey, p.App, app)
			ts := h.getTimers(tk)
			ts.graceTimer = time.AfterFunc(gracePeriod, func() {
				h.graceExpired(userKey, p.App)
			})
			h.mu.Unlock()
			slog.Info("sync succeeded (untracked, grace period)", "slug", slug, "app", p.App)
			return
		}

		// No grace period — create and send step 2 (original behavior)
		app = &trackedAppState{
			Slug:     slug,
			Revision: p.Revision,
			RepoURL:  p.RepoURL,
			Step:     2,
		}
		_ = h.saveApp(ctx, userKey, p.App, app)
		h.mu.Unlock()

		pw := h.clients.Get(userKey)
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pw.CreateActivity(ctx, slug, p.App, h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			h.deleteApp(ctx, userKey, p.App)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity (untracked sync-succeeded)", "slug", slug, "app", p.App)
	} else {
		app.Step = 2
		_ = h.saveApp(ctx, userKey, p.App, app)
		h.mu.Unlock()
	}

	pw := h.clients.Get(userKey)
	step := 2
	total := totalSteps
	url, secondaryURL := h.contentURLs(p.App, p.RepoURL, p.Revision)
	req := pushward.UpdateRequest{
		State: "ONGOING",
		Content: pushward.Content{
			Template:     "pipeline",
			Progress:     float64(step) / float64(total),
			State:        "Rolling out...",
			Icon:         "arrow.triangle.2.circlepath",
			Subtitle:     "ArgoCD \u00b7 " + p.App,
			AccentColor:  "#007AFF",
			CurrentStep:  &step,
			TotalSteps:   &total,
			URL:          url,
			SecondaryURL: secondaryURL,
		},
	}

	if err := pw.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "step", "2/3", "state", "Rolling out...")
}

func (h *Handler) handleDeployed(ctx context.Context, userKey string, p *webhookPayload) {
	slug := slugForApp(p.App)
	tk := timerKey(userKey, p.App)

	h.mu.Lock()
	app, exists, _ := h.loadApp(ctx, userKey, p.App)

	// Completed during grace period — no-op sync, skip entirely
	if exists && app.Pending {
		ts := h.getTimers(tk)
		if ts.graceTimer != nil {
			ts.graceTimer.Stop()
		}
		h.deleteApp(ctx, userKey, p.App)
		delete(h.timers, tk)

		// Record tombstone so a late sync-succeeded is also skipped
		if h.config.SyncGracePeriod > 0 {
			h.setTombstone(ctx, userKey, p.App)
		}

		h.mu.Unlock()
		slog.Info("skipped no-op sync", "slug", slug, "app", p.App)
		return
	}

	if !exists {
		// Untracked deployed
		if h.config.SyncGracePeriod > 0 {
			h.setTombstone(ctx, userKey, p.App)
			h.mu.Unlock()
			slog.Info("recorded untracked deployed", "slug", slug, "app", p.App)
			return
		}

		// No grace period — create and immediately end (original behavior)
		app = &trackedAppState{
			Slug:     slug,
			Revision: p.Revision,
			RepoURL:  p.RepoURL,
			Step:     3,
		}
		_ = h.saveApp(ctx, userKey, p.App, app)
		h.mu.Unlock()

		pw := h.clients.Get(userKey)
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pw.CreateActivity(ctx, slug, p.App, h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			h.deleteApp(ctx, userKey, p.App)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity (untracked deployed)", "slug", slug, "app", p.App)
	} else {
		app.Step = 3
		_ = h.saveApp(ctx, userKey, p.App, app)
		h.mu.Unlock()
	}

	step := 3
	total := totalSteps
	url, secondaryURL := h.contentURLs(p.App, app.RepoURL, app.Revision)
	content := pushward.Content{
		Template:     "pipeline",
		Progress:     1.0,
		State:        "Deployed",
		Icon:         "checkmark.circle.fill",
		Subtitle:     "ArgoCD \u00b7 " + p.App,
		AccentColor:  "#34C759",
		CurrentStep:  &step,
		TotalSteps:   &total,
		URL:          url,
		SecondaryURL: secondaryURL,
	}

	h.scheduleEnd(userKey, p.App, content)
	slog.Info("scheduled end", "slug", slug, "state", "Deployed")
}

func (h *Handler) handleSyncFailed(ctx context.Context, userKey string, p *webhookPayload) {
	slug := slugForApp(p.App)
	tk := timerKey(userKey, p.App)

	h.mu.Lock()
	app, exists, _ := h.loadApp(ctx, userKey, p.App)
	currentStep := 1
	wasPending := false
	if exists {
		currentStep = app.Step
		wasPending = app.Pending
		if app.Pending {
			ts := h.getTimers(tk)
			if ts.graceTimer != nil {
				ts.graceTimer.Stop()
				ts.graceTimer = nil
			}
			app.Pending = false
			_ = h.saveApp(ctx, userKey, p.App, app)
		}
	} else {
		app = &trackedAppState{
			Slug:     slug,
			Revision: p.Revision,
			RepoURL:  p.RepoURL,
			Step:     1,
		}
		_ = h.saveApp(ctx, userKey, p.App, app)
	}
	h.mu.Unlock()

	// Create activity if untracked or was pending (activity never created on PushWard)
	if !exists || wasPending {
		pw := h.clients.Get(userKey)
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pw.CreateActivity(ctx, slug, p.App, h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			h.deleteApp(ctx, userKey, p.App)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity (sync-failed)", "slug", slug, "app", p.App)
	}

	total := totalSteps
	url, secondaryURL := h.contentURLs(p.App, p.RepoURL, p.Revision)
	content := pushward.Content{
		Template:     "pipeline",
		Progress:     float64(currentStep) / float64(total),
		State:        "Sync Failed",
		Icon:         "xmark.circle.fill",
		Subtitle:     "ArgoCD \u00b7 " + p.App,
		AccentColor:  "#FF3B30",
		CurrentStep:  &currentStep,
		TotalSteps:   &total,
		URL:          url,
		SecondaryURL: secondaryURL,
	}

	h.scheduleEnd(userKey, p.App, content)
	slog.Info("scheduled end", "slug", slug, "state", "Sync Failed")
}

func (h *Handler) handleHealthDegraded(ctx context.Context, userKey string, p *webhookPayload) {
	slug := slugForApp(p.App)
	tk := timerKey(userKey, p.App)

	h.mu.Lock()
	app, exists, _ := h.loadApp(ctx, userKey, p.App)
	currentStep := 1
	wasPending := false
	if exists {
		currentStep = app.Step
		wasPending = app.Pending
		if app.Pending {
			ts := h.getTimers(tk)
			if ts.graceTimer != nil {
				ts.graceTimer.Stop()
				ts.graceTimer = nil
			}
			app.Pending = false
			_ = h.saveApp(ctx, userKey, p.App, app)
		}
	} else {
		app = &trackedAppState{
			Slug:     slug,
			Revision: p.Revision,
			RepoURL:  p.RepoURL,
			Step:     1,
		}
		_ = h.saveApp(ctx, userKey, p.App, app)
	}
	h.mu.Unlock()

	// Transient degradation during rolling update (step 2, tracked, not pending):
	// show warning on Dynamic Island but keep the activity alive so deployed can complete it.
	if exists && !wasPending && currentStep == 2 {
		pw := h.clients.Get(userKey)
		step := 2
		total := totalSteps
		url, secondaryURL := h.contentURLs(p.App, p.RepoURL, p.Revision)
		req := pushward.UpdateRequest{
			State: "ONGOING",
			Content: pushward.Content{
				Template:     "pipeline",
				Progress:     float64(step) / float64(total),
				State:        "Degraded",
				Icon:         "exclamationmark.triangle.fill",
				Subtitle:     "ArgoCD \u00b7 " + p.App,
				AccentColor:  "#FF9500",
				CurrentStep:  &step,
				TotalSteps:   &total,
				URL:          url,
				SecondaryURL: secondaryURL,
			},
		}
		if err := pw.UpdateActivity(ctx, slug, req); err != nil {
			slog.Error("failed to update activity", "slug", slug, "error", err)
			return
		}
		slog.Info("updated activity (transient degraded)", "slug", slug, "step", "2/3", "state", "Degraded")
		return
	}

	// Create activity if untracked or was pending (activity never created on PushWard)
	if !exists || wasPending {
		pw := h.clients.Get(userKey)
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pw.CreateActivity(ctx, slug, p.App, h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			h.deleteApp(ctx, userKey, p.App)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity (health-degraded)", "slug", slug, "app", p.App)
	}

	total := totalSteps
	url, secondaryURL := h.contentURLs(p.App, p.RepoURL, p.Revision)
	content := pushward.Content{
		Template:     "pipeline",
		Progress:     float64(currentStep) / float64(total),
		State:        "Degraded",
		Icon:         "exclamationmark.triangle.fill",
		Subtitle:     "ArgoCD \u00b7 " + p.App,
		AccentColor:  "#FF9500",
		CurrentStep:  &currentStep,
		TotalSteps:   &total,
		URL:          url,
		SecondaryURL: secondaryURL,
	}

	h.scheduleEnd(userKey, p.App, content)
	slog.Info("scheduled end", "slug", slug, "state", "Degraded")
}

// graceExpired is called when the sync grace period expires. It creates
// the activity and sends an update for whatever step the sync is currently at.
func (h *Handler) graceExpired(userKey, appName string) {
	tk := timerKey(userKey, appName)

	h.mu.Lock()
	app, ok, _ := h.loadApp(context.Background(), userKey, appName)
	if !ok || !app.Pending {
		h.mu.Unlock()
		return
	}
	app.Pending = false
	_ = h.saveApp(context.Background(), userKey, appName, app)
	ts := h.getTimers(tk)
	ts.graceTimer = nil
	slug := app.Slug
	step := app.Step
	revision := app.Revision
	repoURL := app.RepoURL
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pw := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())
	if err := pw.CreateActivity(ctx, slug, appName, h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "slug", slug, "error", err)
		h.mu.Lock()
		h.deleteApp(ctx, userKey, appName)
		h.mu.Unlock()
		return
	}
	slog.Info("created activity (grace expired)", "slug", slug, "app", appName, "step", step)

	var stateText string
	switch step {
	case 1:
		stateText = "Syncing..."
	case 2:
		stateText = "Rolling out..."
	default:
		stateText = fmt.Sprintf("Step %d", step)
	}

	total := totalSteps
	url, secondaryURL := h.contentURLs(appName, repoURL, revision)
	req := pushward.UpdateRequest{
		State: "ONGOING",
		Content: pushward.Content{
			Template:     "pipeline",
			Progress:     float64(step) / float64(total),
			State:        stateText,
			Icon:         "arrow.triangle.2.circlepath",
			Subtitle:     "ArgoCD \u00b7 " + appName,
			AccentColor:  "#007AFF",
			CurrentStep:  &step,
			TotalSteps:   &total,
			URL:          url,
			SecondaryURL: secondaryURL,
		},
	}

	if err := pw.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "step", fmt.Sprintf("%d/%d", step, total), "state", stateText)
}

// scheduleEnd schedules a two-phase end for an activity:
//   - Phase 1 (after EndDelay): ONGOING update with final content (visible in Dynamic Island)
//   - Phase 2 (EndDisplayTime later): ENDED with same content (dismisses Live Activity)
func (h *Handler) scheduleEnd(userKey, appName string, content pushward.Content) {
	tk := timerKey(userKey, appName)

	h.mu.Lock()
	app, ok, _ := h.loadApp(context.Background(), userKey, appName)
	if !ok {
		h.mu.Unlock()
		return
	}
	slug := app.Slug
	endDelay := h.config.EndDelay
	displayTime := h.config.EndDisplayTime

	ts := h.getTimers(tk)
	ts.endTimer = time.AfterFunc(endDelay, func() {
		pw := h.clients.Get(userKey)

		// Phase 1: ONGOING with final content
		ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
		ongoingReq := pushward.UpdateRequest{
			State:   "ONGOING",
			Content: content,
		}
		if err := pw.UpdateActivity(ctx1, slug, ongoingReq); err != nil {
			slog.Error("failed to update activity (end phase 1)", "slug", slug, "error", err)
		} else {
			slog.Info("updated activity", "slug", slug, "state", content.State)
		}
		cancel1()

		time.Sleep(displayTime)

		// Phase 2: ENDED with same content
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel2()
		endedReq := pushward.UpdateRequest{
			State:   "ENDED",
			Content: content,
		}
		if err := pw.UpdateActivity(ctx2, slug, endedReq); err != nil {
			slog.Error("failed to end activity (end phase 2)", "slug", slug, "error", err)
		} else {
			slog.Info("ended activity", "slug", slug, "state", content.State)
		}

		// Server handles cleanup via ended_ttl — just remove from store and timers
		h.mu.Lock()
		h.deleteApp(context.Background(), userKey, appName)
		delete(h.timers, tk)
		h.mu.Unlock()
	})
	h.mu.Unlock()
}
