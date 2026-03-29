package argocd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
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

// Lock ordering: appLocks (per-app) → mu (graceTimers). Never acquire mu before appLocks.
type Handler struct {
	store       state.Store
	clients     *client.Pool
	config      *config.ArgoCDConfig
	ender       *lifecycle.Ender
	mu          sync.Mutex             // protects graceTimers map only
	appLocks    sync.Map               // "userKey:appName" → *sync.Mutex
	graceTimers map[string]*time.Timer // "userKey:appName" → grace timer
}

// lockApp returns an unlock function for per-app serialization.
func (h *Handler) lockApp(userKey, appName string) func() {
	key := userKey + ":" + appName
	val, _ := h.appLocks.LoadOrStore(key, &sync.Mutex{})
	m := val.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

func NewHandler(store state.Store, clients *client.Pool, cfg *config.ArgoCDConfig) *Handler {
	return &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "argocd", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
		graceTimers: make(map[string]*time.Timer),
	}
}

func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

// StartCleanup starts a background goroutine that periodically removes stale
// entries from graceTimers. Since timers self-clean when they fire, this is a
// safety net for timers whose associated app state is no longer pending.
func (h *Handler) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(h.config.StaleTimeout)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Collect candidates under lock
				h.mu.Lock()
				candidates := make([]string, 0, len(h.graceTimers))
				for tk := range h.graceTimers {
					candidates = append(candidates, tk)
				}
				h.mu.Unlock()

				// Check each candidate outside the lock
				for _, tk := range candidates {
					parts := strings.SplitN(tk, ":", 2)
					if len(parts) != 2 {
						h.mu.Lock()
						if gt, ok := h.graceTimers[tk]; ok {
							gt.Stop()
							delete(h.graceTimers, tk)
						}
						h.mu.Unlock()
						continue
					}
					app, ok, _ := h.loadApp(context.Background(), parts[0], parts[1])
					if !ok || !app.Pending {
						h.mu.Lock()
						if gt, ok := h.graceTimers[tk]; ok {
							gt.Stop()
							delete(h.graceTimers, tk)
						}
						h.mu.Unlock()
					}
				}
			}
		}
	}()
}

func timerKey(userKey, appName string) string {
	return userKey + ":" + appName
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
	if err := h.store.Delete(ctx, "argocd", userKey, appName, ""); err != nil {
		slog.Warn("state store delete failed", "error", err, "provider", "argocd", "tenant", auth.KeyHash(userKey))
	}
}

func (h *Handler) setTombstone(ctx context.Context, userKey, appName string) {
	if err := h.store.Set(ctx, "argocd", userKey, appName, "tombstone", []byte("{}"), h.config.SyncGracePeriod*2); err != nil {
		slog.Warn("state store write failed", "error", err, "provider", "argocd", "tenant", auth.KeyHash(userKey))
	}
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
	if sanitized := text.SanitizeURL(repoURL); sanitized != "" && revision != "" {
		secondaryURL = strings.TrimSuffix(sanitized, ".git") + "/commit/" + revision
	}
	return url, secondaryURL
}

// slugForApp derives a stable, URL-safe activity slug from an ArgoCD app name.
func slugForApp(appName string) string {
	return text.Slug("argocd-", appName)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

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
	tenant := auth.KeyHash(userKey)
	ctx := r.Context()

	switch payload.Event {
	case "sync-running":
		h.handleSyncRunning(ctx, userKey, tenant, &payload)
	case "sync-succeeded":
		h.handleSyncSucceeded(ctx, userKey, tenant, &payload)
	case "deployed":
		h.handleDeployed(ctx, userKey, tenant, &payload)
	case "sync-failed":
		h.handleSyncFailed(ctx, userKey, tenant, &payload)
	case "health-degraded":
		h.handleHealthDegraded(ctx, userKey, tenant, &payload)
	default:
		slog.Warn("unknown event", "event", payload.Event, "app", payload.App, "tenant", tenant)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleSyncRunning(ctx context.Context, userKey, tenant string, p *webhookPayload) {
	slug := slugForApp(p.App)
	tk := timerKey(userKey, p.App)

	unlock := h.lockApp(userKey, p.App)
	defer unlock()

	// If a recent-deploy tombstone exists, the sync already completed and this
	// sync-running arrived out of order — skip it entirely.
	if h.hasTombstone(ctx, userKey, p.App) {
		slog.Info("skipped late sync-running (already deployed)", "slug", slug, "app", p.App, "tenant", tenant)
		return
	}

	app, exists, err := h.loadApp(ctx, userKey, p.App)
	if err != nil {
		slog.Error("failed to load app state", "app", p.App, "error", err, "tenant", tenant)
	}
	needsCreate := !exists || (p.Revision != "" && app.Revision != p.Revision)

	h.ender.StopTimer(userKey, p.App)

	if needsCreate {
		if exists {
			h.mu.Lock()
			if gt, ok := h.graceTimers[tk]; ok {
				gt.Stop()
				delete(h.graceTimers, tk)
			}
			h.mu.Unlock()
		}
		app = &trackedAppState{
			Slug:     slug,
			Revision: p.Revision,
			RepoURL:  p.RepoURL,
			Step:     1,
		}
		if err := h.saveApp(ctx, userKey, p.App, app); err != nil {
			slog.Error("failed to save app state", "app", p.App, "error", err, "tenant", tenant)
		}
	} else {
		app.Step = 1
		if err := h.saveApp(ctx, userKey, p.App, app); err != nil {
			slog.Error("failed to save app state", "app", p.App, "error", err, "tenant", tenant)
		}
	}

	// Grace period: defer activity creation for new or still-pending syncs
	gracePeriod := h.config.SyncGracePeriod
	if gracePeriod > 0 && (needsCreate || app.Pending) {
		app.Pending = true
		if err := h.saveApp(ctx, userKey, p.App, app); err != nil {
			slog.Error("failed to save app state", "app", p.App, "error", err, "tenant", tenant)
		}
		h.mu.Lock()
		if gt, ok := h.graceTimers[tk]; ok {
			gt.Stop()
		}
		h.graceTimers[tk] = time.AfterFunc(gracePeriod, func() {
			h.graceExpired(userKey, p.App)
		})
		h.mu.Unlock()
		slog.Info("sync started (grace period)", "slug", slug, "app", p.App, "grace", gracePeriod, "tenant", tenant)
		return
	}

	pw := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if needsCreate {
		if err := pw.CreateActivity(ctx, slug, p.App, h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err, "tenant", tenant)
			h.deleteApp(ctx, userKey, p.App)
			return
		}
		slog.Info("created activity", "slug", slug, "app", p.App, "tenant", tenant)
	}

	step := 1
	total := totalSteps
	url, secondaryURL := h.contentURLs(p.App, p.RepoURL, p.Revision)
	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:     "steps",
			Progress:     float64(step) / float64(total),
			State:        "Syncing...",
			Icon:         "arrow.triangle.2.circlepath",
			Subtitle:     "ArgoCD \u00b7 " + p.App,
			AccentColor:  pushward.ColorBlue,
			CurrentStep:  &step,
			TotalSteps:   &total,
			URL:          url,
			SecondaryURL: secondaryURL,
		},
	}

	if err := pw.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err, "tenant", tenant)
		return
	}
	slog.Info("updated activity", "slug", slug, "step", "1/3", "state", "Syncing...", "tenant", tenant)
}

func (h *Handler) handleSyncSucceeded(ctx context.Context, userKey, tenant string, p *webhookPayload) {
	slug := slugForApp(p.App)
	tk := timerKey(userKey, p.App)

	unlock := h.lockApp(userKey, p.App)
	defer unlock()

	app, exists, err := h.loadApp(ctx, userKey, p.App)
	if err != nil {
		slog.Error("failed to load app state", "app", p.App, "error", err, "tenant", tenant)
	}

	// Tracked and still in grace period — just advance step, don't touch PushWard
	if exists && app.Pending {
		app.Step = 2
		if err := h.saveApp(ctx, userKey, p.App, app); err != nil {
			slog.Error("failed to save app state", "app", p.App, "error", err, "tenant", tenant)
		}
		slog.Info("sync succeeded (grace period)", "slug", slug, "app", p.App, "tenant", tenant)
		return
	}

	if !exists {
		// Untracked (bridge restart)
		gracePeriod := h.config.SyncGracePeriod
		if gracePeriod > 0 {
			// If deployed already arrived (out-of-order events), this is a no-op.
			if h.hasTombstone(ctx, userKey, p.App) {
				slog.Info("skipped no-op sync (deployed arrived first)", "slug", slug, "app", p.App, "tenant", tenant)
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
			if err := h.saveApp(ctx, userKey, p.App, app); err != nil {
				slog.Error("failed to save app state", "app", p.App, "error", err, "tenant", tenant)
			}
			h.mu.Lock()
			h.graceTimers[tk] = time.AfterFunc(gracePeriod, func() {
				h.graceExpired(userKey, p.App)
			})
			h.mu.Unlock()
			slog.Info("sync succeeded (untracked, grace period)", "slug", slug, "app", p.App, "tenant", tenant)
			return
		}

		// No grace period — create and send step 2 (original behavior)
		app = &trackedAppState{
			Slug:     slug,
			Revision: p.Revision,
			RepoURL:  p.RepoURL,
			Step:     2,
		}
		if err := h.saveApp(ctx, userKey, p.App, app); err != nil {
			slog.Error("failed to save app state", "app", p.App, "error", err, "tenant", tenant)
		}

		pw := h.clients.Get(userKey)
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pw.CreateActivity(ctx, slug, p.App, h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err, "tenant", tenant)
			h.deleteApp(ctx, userKey, p.App)
			return
		}
		slog.Info("created activity (untracked sync-succeeded)", "slug", slug, "app", p.App, "tenant", tenant)
	} else {
		app.Step = 2
		if err := h.saveApp(ctx, userKey, p.App, app); err != nil {
			slog.Error("failed to save app state", "app", p.App, "error", err, "tenant", tenant)
		}
	}

	pw := h.clients.Get(userKey)
	step := 2
	total := totalSteps
	url, secondaryURL := h.contentURLs(p.App, p.RepoURL, p.Revision)
	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:     "steps",
			Progress:     float64(step) / float64(total),
			State:        "Rolling out...",
			Icon:         "arrow.triangle.2.circlepath",
			Subtitle:     "ArgoCD \u00b7 " + p.App,
			AccentColor:  pushward.ColorBlue,
			CurrentStep:  &step,
			TotalSteps:   &total,
			URL:          url,
			SecondaryURL: secondaryURL,
		},
	}

	if err := pw.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err, "tenant", tenant)
		return
	}
	slog.Info("updated activity", "slug", slug, "step", "2/3", "state", "Rolling out...", "tenant", tenant)
}

func (h *Handler) handleDeployed(ctx context.Context, userKey, tenant string, p *webhookPayload) {
	slug := slugForApp(p.App)
	tk := timerKey(userKey, p.App)

	unlock := h.lockApp(userKey, p.App)
	defer unlock()

	app, exists, err := h.loadApp(ctx, userKey, p.App)
	if err != nil {
		slog.Error("failed to load app state", "app", p.App, "error", err, "tenant", tenant)
	}

	// Completed during grace period — no-op sync, skip entirely
	if exists && app.Pending {
		h.mu.Lock()
		if gt, ok := h.graceTimers[tk]; ok {
			gt.Stop()
			delete(h.graceTimers, tk)
		}
		h.mu.Unlock()
		h.deleteApp(ctx, userKey, p.App)

		// Record tombstone so a late sync-succeeded is also skipped
		if h.config.SyncGracePeriod > 0 {
			h.setTombstone(ctx, userKey, p.App)
		}

		slog.Info("skipped no-op sync", "slug", slug, "app", p.App, "tenant", tenant)
		return
	}

	if !exists {
		// Untracked deployed
		if h.config.SyncGracePeriod > 0 {
			h.setTombstone(ctx, userKey, p.App)
			slog.Info("recorded untracked deployed", "slug", slug, "app", p.App, "tenant", tenant)
			return
		}

		// No grace period — create and immediately end (original behavior)
		app = &trackedAppState{
			Slug:     slug,
			Revision: p.Revision,
			RepoURL:  p.RepoURL,
			Step:     3,
		}
		if err := h.saveApp(ctx, userKey, p.App, app); err != nil {
			slog.Error("failed to save app state", "app", p.App, "error", err, "tenant", tenant)
		}

		pw := h.clients.Get(userKey)
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pw.CreateActivity(ctx, slug, p.App, h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err, "tenant", tenant)
			h.deleteApp(ctx, userKey, p.App)
			return
		}
		slog.Info("created activity (untracked deployed)", "slug", slug, "app", p.App, "tenant", tenant)
	} else {
		app.Step = 3
		if err := h.saveApp(ctx, userKey, p.App, app); err != nil {
			slog.Error("failed to save app state", "app", p.App, "error", err, "tenant", tenant)
		}
	}

	step := 3
	total := totalSteps
	url, secondaryURL := h.contentURLs(p.App, app.RepoURL, app.Revision)
	content := pushward.Content{
		Template:     "steps",
		Progress:     1.0,
		State:        "Deployed",
		Icon:         "checkmark.circle.fill",
		Subtitle:     "ArgoCD \u00b7 " + p.App,
		AccentColor:  pushward.ColorGreen,
		CurrentStep:  &step,
		TotalSteps:   &total,
		URL:          url,
		SecondaryURL: secondaryURL,
	}

	h.ender.ScheduleEnd(userKey, p.App, slug, content)
	slog.Info("scheduled end", "slug", slug, "state", "Deployed", "tenant", tenant)
}

// errorPreamble loads app state, clears grace timers if pending, and ensures
// an activity exists on PushWard. It is the shared entry path for
// handleSyncFailed and handleHealthDegraded.
type errorPreambleResult struct {
	slug        string
	currentStep int
	exists      bool
	wasPending  bool
}

func (h *Handler) errorPreamble(ctx context.Context, userKey, tenant string, p *webhookPayload, event string) (*errorPreambleResult, bool) {
	slug := slugForApp(p.App)
	tk := timerKey(userKey, p.App)

	app, exists, err := h.loadApp(ctx, userKey, p.App)
	if err != nil {
		slog.Error("failed to load app state", "app", p.App, "error", err, "tenant", tenant)
	}
	currentStep := 1
	wasPending := false
	if exists {
		currentStep = app.Step
		wasPending = app.Pending
		if app.Pending {
			h.mu.Lock()
			if gt, ok := h.graceTimers[tk]; ok {
				gt.Stop()
				delete(h.graceTimers, tk)
			}
			h.mu.Unlock()
			app.Pending = false
			if err := h.saveApp(ctx, userKey, p.App, app); err != nil {
				slog.Error("failed to save app state", "app", p.App, "error", err, "tenant", tenant)
			}
		}
	} else {
		app = &trackedAppState{
			Slug:     slug,
			Revision: p.Revision,
			RepoURL:  p.RepoURL,
			Step:     1,
		}
		if err := h.saveApp(ctx, userKey, p.App, app); err != nil {
			slog.Error("failed to save app state", "app", p.App, "error", err, "tenant", tenant)
		}
	}

	// Create activity if untracked or was pending (activity never created on PushWard)
	if !exists || wasPending {
		pw := h.clients.Get(userKey)
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pw.CreateActivity(ctx, slug, p.App, h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err, "tenant", tenant)
			h.deleteApp(ctx, userKey, p.App)
			return nil, false
		}
		slog.Info("created activity ("+event+")", "slug", slug, "app", p.App, "tenant", tenant)
	}

	return &errorPreambleResult{
		slug:        slug,
		currentStep: currentStep,
		exists:      exists,
		wasPending:  wasPending,
	}, true
}

func (h *Handler) handleSyncFailed(ctx context.Context, userKey, tenant string, p *webhookPayload) {
	unlock := h.lockApp(userKey, p.App)
	defer unlock()

	res, ok := h.errorPreamble(ctx, userKey, tenant, p, "sync-failed")
	if !ok {
		return
	}

	total := totalSteps
	url, secondaryURL := h.contentURLs(p.App, p.RepoURL, p.Revision)
	content := pushward.Content{
		Template:     "steps",
		Progress:     float64(res.currentStep) / float64(total),
		State:        "Sync Failed",
		Icon:         "xmark.circle.fill",
		Subtitle:     "ArgoCD \u00b7 " + p.App,
		AccentColor:  pushward.ColorRed,
		CurrentStep:  &res.currentStep,
		TotalSteps:   &total,
		URL:          url,
		SecondaryURL: secondaryURL,
	}

	h.ender.ScheduleEnd(userKey, p.App, res.slug, content)
	slog.Info("scheduled end", "slug", res.slug, "state", "Sync Failed", "tenant", tenant)
}

func (h *Handler) handleHealthDegraded(ctx context.Context, userKey, tenant string, p *webhookPayload) {
	slug := slugForApp(p.App)

	unlock := h.lockApp(userKey, p.App)
	defer unlock()

	// Transient degradation during rolling update (step 2, tracked, not pending):
	// must check before errorPreamble which would create an activity.
	app, exists, err := h.loadApp(ctx, userKey, p.App)
	if err != nil {
		slog.Error("failed to load app state", "app", p.App, "error", err, "tenant", tenant)
	}
	isTransient := exists && !app.Pending && app.Step == 2

	if isTransient {
		pw := h.clients.Get(userKey)
		step := 2
		total := totalSteps
		url, secondaryURL := h.contentURLs(p.App, p.RepoURL, p.Revision)
		req := pushward.UpdateRequest{
			State: pushward.StateOngoing,
			Content: pushward.Content{
				Template:     "steps",
				Progress:     float64(step) / float64(total),
				State:        "Degraded",
				Icon:         "exclamationmark.triangle.fill",
				Subtitle:     "ArgoCD \u00b7 " + p.App,
				AccentColor:  pushward.ColorOrange,
				CurrentStep:  &step,
				TotalSteps:   &total,
				URL:          url,
				SecondaryURL: secondaryURL,
			},
		}
		if err := pw.UpdateActivity(ctx, slug, req); err != nil {
			slog.Error("failed to update activity", "slug", slug, "error", err, "tenant", tenant)
			return
		}
		slog.Info("updated activity (transient degraded)", "slug", slug, "step", "2/3", "state", "Degraded", "tenant", tenant)
		return
	}

	res, ok := h.errorPreamble(ctx, userKey, tenant, p, "health-degraded")
	if !ok {
		return
	}

	total := totalSteps
	url, secondaryURL := h.contentURLs(p.App, p.RepoURL, p.Revision)
	content := pushward.Content{
		Template:     "steps",
		Progress:     float64(res.currentStep) / float64(total),
		State:        "Degraded",
		Icon:         "exclamationmark.triangle.fill",
		Subtitle:     "ArgoCD \u00b7 " + p.App,
		AccentColor:  pushward.ColorOrange,
		CurrentStep:  &res.currentStep,
		TotalSteps:   &total,
		URL:          url,
		SecondaryURL: secondaryURL,
	}

	h.ender.ScheduleEnd(userKey, p.App, res.slug, content)
	slog.Info("scheduled end", "slug", res.slug, "state", "Degraded", "tenant", tenant)
}

// graceExpired is called when the sync grace period expires. It creates
// the activity and sends an update for whatever step the sync is currently at.
func (h *Handler) graceExpired(userKey, appName string) {
	tk := timerKey(userKey, appName)

	unlock := h.lockApp(userKey, appName)
	defer unlock()

	app, ok, err := h.loadApp(context.Background(), userKey, appName)
	if err != nil {
		slog.Error("failed to load app state", "app", appName, "error", err)
	}
	if !ok || !app.Pending {
		return
	}
	app.Pending = false
	if err := h.saveApp(context.Background(), userKey, appName, app); err != nil {
		slog.Error("failed to save app state", "app", appName, "error", err)
	}
	h.mu.Lock()
	delete(h.graceTimers, tk)
	h.mu.Unlock()
	slug := app.Slug
	step := app.Step
	revision := app.Revision
	repoURL := app.RepoURL

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pw := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())
	if err := pw.CreateActivity(ctx, slug, appName, h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "slug", slug, "error", err)
		h.deleteApp(ctx, userKey, appName)
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
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:     "steps",
			Progress:     float64(step) / float64(total),
			State:        stateText,
			Icon:         "arrow.triangle.2.circlepath",
			Subtitle:     "ArgoCD \u00b7 " + appName,
			AccentColor:  pushward.ColorBlue,
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
