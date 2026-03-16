package handler

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

	"github.com/mac-lucky/pushward-integrations/argocd/internal/argocd"
	"github.com/mac-lucky/pushward-integrations/argocd/internal/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

const totalSteps = 3

type Handler struct {
	client        *pushward.Client
	config        *config.Config
	mu            sync.Mutex
	apps          map[string]*trackedApp // keyed by app name
	recentDeploys map[string]*time.Timer // untracked deploys awaiting a matching sync-succeeded
}

type trackedApp struct {
	slug       string
	appName    string
	revision   string
	repoURL    string
	step       int
	pending    bool        // true while in sync grace period (activity not yet created)
	graceTimer *time.Timer // fires when grace period expires
	endTimer   *time.Timer
}

func New(client *pushward.Client, cfg *config.Config) *Handler {
	return &Handler{
		client:        client,
		config:        cfg,
		apps:          make(map[string]*trackedApp),
		recentDeploys: make(map[string]*time.Timer),
	}
}

// contentURLs returns the url and secondary_url fields for a given app and payload.
func (h *Handler) contentURLs(appName, repoURL, revision string) (string, string) {
	var url, secondaryURL string
	if h.config.ArgoCD.URL != "" {
		url = h.config.ArgoCD.URL + "/applications/argocd/" + appName
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

func (h *Handler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Webhook secret validation
	if h.config.ArgoCD.WebhookSecret != "" {
		got := r.Header.Get("X-Webhook-Secret")
		if subtle.ConstantTimeCompare([]byte(got), []byte(h.config.ArgoCD.WebhookSecret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var payload argocd.WebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		slog.Error("failed to decode webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	if payload.App == "" || payload.Event == "" {
		http.Error(w, "missing app or event", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	switch payload.Event {
	case "sync-running":
		h.handleSyncRunning(ctx, &payload)
	case "sync-succeeded":
		h.handleSyncSucceeded(ctx, &payload)
	case "deployed":
		h.handleDeployed(ctx, &payload)
	case "sync-failed":
		h.handleSyncFailed(ctx, &payload)
	case "health-degraded":
		h.handleHealthDegraded(ctx, &payload)
	default:
		slog.Warn("unknown event", "event", payload.Event, "app", payload.App)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleSyncRunning(ctx context.Context, p *argocd.WebhookPayload) {
	slug := slugForApp(p.App)

	h.mu.Lock()
	// If a recent-deploy marker exists, the sync already completed and this
	// sync-running arrived out of order — skip it entirely.
	if t, ok := h.recentDeploys[p.App]; ok {
		t.Stop()
		delete(h.recentDeploys, p.App)
		h.mu.Unlock()
		slog.Info("skipped late sync-running (already deployed)", "slug", slug, "app", p.App)
		return
	}

	app, exists := h.apps[p.App]
	needsCreate := !exists || (p.Revision != "" && app.revision != p.Revision)

	if needsCreate {
		if exists {
			// New revision on existing app — reset tracking
			if app.graceTimer != nil {
				app.graceTimer.Stop()
			}
			if app.endTimer != nil {
				app.endTimer.Stop()
			}
		}
		app = &trackedApp{
			slug:     slug,
			appName:  p.App,
			revision: p.Revision,
			repoURL:  p.RepoURL,
			step:     1,
		}
		h.apps[p.App] = app
	} else {
		app.step = 1
	}

	if app.endTimer != nil {
		app.endTimer.Stop()
		app.endTimer = nil
	}

	// Grace period: defer activity creation for new or still-pending syncs
	gracePeriod := h.config.ArgoCD.SyncGracePeriod
	if gracePeriod > 0 && (needsCreate || app.pending) {
		app.pending = true
		if app.graceTimer != nil {
			app.graceTimer.Stop()
		}
		app.graceTimer = time.AfterFunc(gracePeriod, func() {
			h.graceExpired(p.App)
		})
		h.mu.Unlock()
		slog.Info("sync started (grace period)", "slug", slug, "app", p.App, "grace", gracePeriod)
		return
	}

	h.mu.Unlock()

	endedTTL := int(h.config.PushWard.CleanupDelay.Seconds())
	staleTTL := int(h.config.PushWard.StaleTimeout.Seconds())

	if needsCreate {
		if err := h.client.CreateActivity(ctx, slug, p.App, h.config.PushWard.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			delete(h.apps, p.App)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity", "slug", slug, "app", p.App)
	}

	step := 1
	total := totalSteps
	url, secondaryURL := h.contentURLs(p.App, p.RepoURL, p.Revision)
	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
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

	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "step", "1/3", "state", "Syncing...")
}

func (h *Handler) handleSyncSucceeded(ctx context.Context, p *argocd.WebhookPayload) {
	slug := slugForApp(p.App)

	h.mu.Lock()
	app, exists := h.apps[p.App]

	// Tracked and still in grace period — just advance step, don't touch PushWard
	if exists && app.pending {
		app.step = 2
		h.mu.Unlock()
		slog.Info("sync succeeded (grace period)", "slug", slug, "app", p.App)
		return
	}

	if !exists {
		// Untracked (bridge restart)
		gracePeriod := h.config.ArgoCD.SyncGracePeriod
		if gracePeriod > 0 {
			// If deployed already arrived (out-of-order events), this is a no-op.
			// Leave the marker in place so a late sync-running also detects it.
			if _, ok := h.recentDeploys[p.App]; ok {
				h.mu.Unlock()
				slog.Info("skipped no-op sync (deployed arrived first)", "slug", slug, "app", p.App)
				return
			}

			// Start grace period at step 2 — if deployed comes quickly, skip
			app = &trackedApp{
				slug:     slug,
				appName:  p.App,
				revision: p.Revision,
				repoURL:  p.RepoURL,
				step:     2,
				pending:  true,
			}
			app.graceTimer = time.AfterFunc(gracePeriod, func() {
				h.graceExpired(p.App)
			})
			h.apps[p.App] = app
			h.mu.Unlock()
			slog.Info("sync succeeded (untracked, grace period)", "slug", slug, "app", p.App)
			return
		}

		// No grace period — create and send step 2 (original behavior)
		app = &trackedApp{
			slug:     slug,
			appName:  p.App,
			revision: p.Revision,
			repoURL:  p.RepoURL,
			step:     2,
		}
		h.apps[p.App] = app
		h.mu.Unlock()

		endedTTL := int(h.config.PushWard.CleanupDelay.Seconds())
		staleTTL := int(h.config.PushWard.StaleTimeout.Seconds())
		if err := h.client.CreateActivity(ctx, slug, p.App, h.config.PushWard.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			delete(h.apps, p.App)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity (untracked sync-succeeded)", "slug", slug, "app", p.App)
	} else {
		app.step = 2
		h.mu.Unlock()
	}

	step := 2
	total := totalSteps
	url, secondaryURL := h.contentURLs(p.App, p.RepoURL, p.Revision)
	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
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

	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "step", "2/3", "state", "Rolling out...")
}

func (h *Handler) handleDeployed(ctx context.Context, p *argocd.WebhookPayload) {
	slug := slugForApp(p.App)

	h.mu.Lock()
	app, exists := h.apps[p.App]

	// Completed during grace period — no-op sync, skip entirely
	if exists && app.pending {
		if app.graceTimer != nil {
			app.graceTimer.Stop()
		}
		delete(h.apps, p.App)

		// Record in recentDeploys so a late sync-succeeded is also skipped
		if h.config.ArgoCD.SyncGracePeriod > 0 {
			if t, ok := h.recentDeploys[p.App]; ok {
				t.Stop()
			}
			h.recentDeploys[p.App] = time.AfterFunc(h.config.ArgoCD.SyncGracePeriod*2, func() {
				h.mu.Lock()
				delete(h.recentDeploys, p.App)
				h.mu.Unlock()
			})
		}

		h.mu.Unlock()
		slog.Info("skipped no-op sync", "slug", slug, "app", p.App)
		return
	}

	if !exists {
		// Untracked deployed
		if h.config.ArgoCD.SyncGracePeriod > 0 {
			// Record the deploy so a subsequent sync-succeeded can detect
			// that this was a no-op (deployed arrived before sync-succeeded).
			if t, ok := h.recentDeploys[p.App]; ok {
				t.Stop()
			}
			h.recentDeploys[p.App] = time.AfterFunc(h.config.ArgoCD.SyncGracePeriod*2, func() {
				h.mu.Lock()
				delete(h.recentDeploys, p.App)
				h.mu.Unlock()
			})
			h.mu.Unlock()
			slog.Info("recorded untracked deployed", "slug", slug, "app", p.App)
			return
		}

		// No grace period — create and immediately end (original behavior)
		app = &trackedApp{
			slug:     slug,
			appName:  p.App,
			revision: p.Revision,
			repoURL:  p.RepoURL,
			step:     3,
		}
		h.apps[p.App] = app
		h.mu.Unlock()

		endedTTL := int(h.config.PushWard.CleanupDelay.Seconds())
		staleTTL := int(h.config.PushWard.StaleTimeout.Seconds())
		if err := h.client.CreateActivity(ctx, slug, p.App, h.config.PushWard.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			delete(h.apps, p.App)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity (untracked deployed)", "slug", slug, "app", p.App)
	} else {
		app.step = 3
		h.mu.Unlock()
	}

	step := 3
	total := totalSteps
	url, secondaryURL := h.contentURLs(p.App, p.RepoURL, p.Revision)
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

	h.scheduleEnd(p.App, content)
	slog.Info("scheduled end", "slug", slug, "state", "Deployed")
}

func (h *Handler) handleSyncFailed(ctx context.Context, p *argocd.WebhookPayload) {
	slug := slugForApp(p.App)

	h.mu.Lock()
	app, exists := h.apps[p.App]
	currentStep := 1
	wasPending := false
	if exists {
		currentStep = app.step
		wasPending = app.pending
		if app.pending {
			if app.graceTimer != nil {
				app.graceTimer.Stop()
				app.graceTimer = nil
			}
			app.pending = false
		}
	} else {
		app = &trackedApp{
			slug:     slug,
			appName:  p.App,
			revision: p.Revision,
			repoURL:  p.RepoURL,
			step:     1,
		}
		h.apps[p.App] = app
	}
	h.mu.Unlock()

	// Create activity if untracked or was pending (activity never created on PushWard)
	if !exists || wasPending {
		endedTTL := int(h.config.PushWard.CleanupDelay.Seconds())
		staleTTL := int(h.config.PushWard.StaleTimeout.Seconds())
		if err := h.client.CreateActivity(ctx, slug, p.App, h.config.PushWard.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			delete(h.apps, p.App)
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

	h.scheduleEnd(p.App, content)
	slog.Info("scheduled end", "slug", slug, "state", "Sync Failed")
}

func (h *Handler) handleHealthDegraded(ctx context.Context, p *argocd.WebhookPayload) {
	slug := slugForApp(p.App)

	h.mu.Lock()
	app, exists := h.apps[p.App]
	currentStep := 1
	wasPending := false
	if exists {
		currentStep = app.step
		wasPending = app.pending
		if app.pending {
			if app.graceTimer != nil {
				app.graceTimer.Stop()
				app.graceTimer = nil
			}
			app.pending = false
		}
	} else {
		app = &trackedApp{
			slug:     slug,
			appName:  p.App,
			revision: p.Revision,
			repoURL:  p.RepoURL,
			step:     1,
		}
		h.apps[p.App] = app
	}
	h.mu.Unlock()

	// Transient degradation during rolling update (step 2, tracked, not pending):
	// show warning on Dynamic Island but keep the activity alive so deployed can complete it.
	if exists && !wasPending && currentStep == 2 {
		step := 2
		total := totalSteps
		url, secondaryURL := h.contentURLs(p.App, p.RepoURL, p.Revision)
		req := pushward.UpdateRequest{
			State: pushward.StateOngoing,
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
		if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
			slog.Error("failed to update activity", "slug", slug, "error", err)
			return
		}
		slog.Info("updated activity (transient degraded)", "slug", slug, "step", "2/3", "state", "Degraded")
		return
	}

	// Create activity if untracked or was pending (activity never created on PushWard)
	if !exists || wasPending {
		endedTTL := int(h.config.PushWard.CleanupDelay.Seconds())
		staleTTL := int(h.config.PushWard.StaleTimeout.Seconds())
		if err := h.client.CreateActivity(ctx, slug, p.App, h.config.PushWard.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			delete(h.apps, p.App)
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

	h.scheduleEnd(p.App, content)
	slog.Info("scheduled end", "slug", slug, "state", "Degraded")
}

// graceExpired is called when the sync grace period expires. It creates
// the activity and sends an update for whatever step the sync is currently at.
func (h *Handler) graceExpired(appName string) {
	h.mu.Lock()
	app, ok := h.apps[appName]
	if !ok || !app.pending {
		h.mu.Unlock()
		return
	}
	app.pending = false
	app.graceTimer = nil
	slug := app.slug
	step := app.step
	revision := app.revision
	repoURL := app.repoURL
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	endedTTL := int(h.config.PushWard.CleanupDelay.Seconds())
	staleTTL := int(h.config.PushWard.StaleTimeout.Seconds())
	if err := h.client.CreateActivity(ctx, slug, appName, h.config.PushWard.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "slug", slug, "error", err)
		h.mu.Lock()
		delete(h.apps, appName)
		h.mu.Unlock()
		return
	}
	slog.Info("created activity (grace expired)", "slug", slug, "app", appName, "step", step)

	var state string
	switch step {
	case 1:
		state = "Syncing..."
	case 2:
		state = "Rolling out..."
	default:
		state = fmt.Sprintf("Step %d", step)
	}

	total := totalSteps
	url, secondaryURL := h.contentURLs(appName, repoURL, revision)
	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:     "pipeline",
			Progress:     float64(step) / float64(total),
			State:        state,
			Icon:         "arrow.triangle.2.circlepath",
			Subtitle:     "ArgoCD \u00b7 " + appName,
			AccentColor:  "#007AFF",
			CurrentStep:  &step,
			TotalSteps:   &total,
			URL:          url,
			SecondaryURL: secondaryURL,
		},
	}

	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "step", fmt.Sprintf("%d/%d", step, total), "state", state)
}

// scheduleEnd schedules a two-phase end for an activity:
//   - Phase 1 (after EndDelay): ONGOING update with final content (visible in Dynamic Island)
//   - Phase 2 (EndDisplayTime later): ENDED with same content (dismisses Live Activity)
//
// This gives iOS time to register the push-update token after push-to-start,
// and ensures the Dynamic Island shows the final state before dismissal.
func (h *Handler) scheduleEnd(appName string, content pushward.Content) {
	h.mu.Lock()
	app, ok := h.apps[appName]
	if !ok {
		h.mu.Unlock()
		return
	}
	slug := app.slug
	endDelay := h.config.PushWard.EndDelay
	displayTime := h.config.PushWard.EndDisplayTime
	app.endTimer = time.AfterFunc(endDelay, func() {
		// Phase 1: ONGOING with final content
		ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
		ongoingReq := pushward.UpdateRequest{
			State:   pushward.StateOngoing,
			Content: content,
		}
		if err := h.client.UpdateActivity(ctx1, slug, ongoingReq); err != nil {
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
			State:   pushward.StateEnded,
			Content: content,
		}
		if err := h.client.UpdateActivity(ctx2, slug, endedReq); err != nil {
			slog.Error("failed to end activity (end phase 2)", "slug", slug, "error", err)
		} else {
			slog.Info("ended activity", "slug", slug, "state", content.State)
		}

		// Server handles cleanup via ended_ttl — just remove from local map
		h.mu.Lock()
		delete(h.apps, appName)
		h.mu.Unlock()
	})
	h.mu.Unlock()
}
