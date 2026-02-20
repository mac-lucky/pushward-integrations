package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-docker/argocd/internal/argocd"
	"github.com/mac-lucky/pushward-docker/argocd/internal/config"
	"github.com/mac-lucky/pushward-docker/argocd/internal/pushward"
)

const totalSteps = 3

type Handler struct {
	client *pushward.Client
	config *config.Config
	mu     sync.Mutex
	apps   map[string]*trackedApp // keyed by app name
}

type trackedApp struct {
	slug         string
	appName      string
	revision     string
	step         int
	cleanupTimer *time.Timer
	staleTimer   *time.Timer
}

func New(client *pushward.Client, cfg *config.Config) *Handler {
	return &Handler{
		client: client,
		config: cfg,
		apps:   make(map[string]*trackedApp),
	}
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
		if r.Header.Get("X-Webhook-Secret") != h.config.ArgoCD.WebhookSecret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

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
	app, exists := h.apps[p.App]
	needsCreate := !exists || (p.Revision != "" && app.revision != p.Revision)

	if needsCreate {
		if exists {
			// New revision on existing app — reset tracking
			if app.cleanupTimer != nil {
				app.cleanupTimer.Stop()
			}
			if app.staleTimer != nil {
				app.staleTimer.Stop()
			}
		}
		app = &trackedApp{
			slug:     slug,
			appName:  p.App,
			revision: p.Revision,
			step:     1,
		}
		h.apps[p.App] = app
	} else {
		// Same revision re-fire, reset stale timer
		app.step = 1
		if app.staleTimer != nil {
			app.staleTimer.Stop()
			app.staleTimer = nil
		}
	}

	if app.cleanupTimer != nil {
		app.cleanupTimer.Stop()
		app.cleanupTimer = nil
	}
	h.mu.Unlock()

	if needsCreate {
		if err := h.client.CreateActivity(ctx, slug, p.App, h.config.PushWard.Priority); err != nil {
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
	req := pushward.UpdateRequest{
		State: "ONGOING",
		Content: pushward.Content{
			Template:    "pipeline",
			Progress:    float64(step) / float64(total),
			State:       "Syncing...",
			Icon:        "arrow.triangle.2.circlepath",
			Subtitle:    "ArgoCD \u00b7 " + p.App,
			AccentColor: "#007AFF",
			CurrentStep: &step,
			TotalSteps:  &total,
		},
	}

	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "step", "1/3", "state", "Syncing...")

	h.mu.Lock()
	if a, ok := h.apps[p.App]; ok {
		a.staleTimer = time.AfterFunc(h.config.PushWard.StaleTimeout, func() {
			h.forceEnd(p.App)
		})
	}
	h.mu.Unlock()
}

func (h *Handler) handleSyncSucceeded(ctx context.Context, p *argocd.WebhookPayload) {
	slug := slugForApp(p.App)

	h.mu.Lock()
	app, exists := h.apps[p.App]
	if !exists {
		// Untracked (bridge restart) — create and send step 2
		app = &trackedApp{
			slug:     slug,
			appName:  p.App,
			revision: p.Revision,
			step:     2,
		}
		h.apps[p.App] = app
		h.mu.Unlock()

		if err := h.client.CreateActivity(ctx, slug, p.App, h.config.PushWard.Priority); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			delete(h.apps, p.App)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity (untracked sync-succeeded)", "slug", slug, "app", p.App)
	} else {
		app.step = 2
		if app.staleTimer != nil {
			app.staleTimer.Stop()
			app.staleTimer = nil
		}
		h.mu.Unlock()
	}

	step := 2
	total := totalSteps
	req := pushward.UpdateRequest{
		State: "ONGOING",
		Content: pushward.Content{
			Template:    "pipeline",
			Progress:    float64(step) / float64(total),
			State:       "Rolling out...",
			Icon:        "arrow.triangle.2.circlepath",
			Subtitle:    "ArgoCD \u00b7 " + p.App,
			AccentColor: "#007AFF",
			CurrentStep: &step,
			TotalSteps:  &total,
		},
	}

	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "step", "2/3", "state", "Rolling out...")

	h.mu.Lock()
	if a, ok := h.apps[p.App]; ok {
		a.staleTimer = time.AfterFunc(h.config.PushWard.StaleTimeout, func() {
			h.forceEnd(p.App)
		})
	}
	h.mu.Unlock()
}

func (h *Handler) handleDeployed(ctx context.Context, p *argocd.WebhookPayload) {
	slug := slugForApp(p.App)

	h.mu.Lock()
	app, exists := h.apps[p.App]
	if !exists {
		// Untracked — create and immediately end
		app = &trackedApp{
			slug:     slug,
			appName:  p.App,
			revision: p.Revision,
			step:     3,
		}
		h.apps[p.App] = app
		h.mu.Unlock()

		if err := h.client.CreateActivity(ctx, slug, p.App, h.config.PushWard.Priority); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			delete(h.apps, p.App)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity (untracked deployed)", "slug", slug, "app", p.App)
	} else {
		app.step = 3
		if app.staleTimer != nil {
			app.staleTimer.Stop()
			app.staleTimer = nil
		}
		h.mu.Unlock()
	}

	step := 3
	total := totalSteps
	req := pushward.UpdateRequest{
		State: "ENDED",
		Content: pushward.Content{
			Template:    "pipeline",
			Progress:    1.0,
			State:       "Deployed",
			Icon:        "checkmark.circle.fill",
			Subtitle:    "ArgoCD \u00b7 " + p.App,
			AccentColor: "#34C759",
			CurrentStep: &step,
			TotalSteps:  &total,
		},
	}

	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("ended activity", "slug", slug, "state", "Deployed")

	h.mu.Lock()
	if a, ok := h.apps[p.App]; ok {
		a.cleanupTimer = time.AfterFunc(h.config.PushWard.CleanupDelay, func() {
			h.cleanup(p.App)
		})
	}
	h.mu.Unlock()
}

func (h *Handler) handleSyncFailed(ctx context.Context, p *argocd.WebhookPayload) {
	slug := slugForApp(p.App)

	h.mu.Lock()
	app, exists := h.apps[p.App]
	currentStep := 1
	if exists {
		currentStep = app.step
		if app.staleTimer != nil {
			app.staleTimer.Stop()
			app.staleTimer = nil
		}
	} else {
		app = &trackedApp{
			slug:     slug,
			appName:  p.App,
			revision: p.Revision,
			step:     1,
		}
		h.apps[p.App] = app
	}
	h.mu.Unlock()

	if !exists {
		if err := h.client.CreateActivity(ctx, slug, p.App, h.config.PushWard.Priority); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			delete(h.apps, p.App)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity (untracked sync-failed)", "slug", slug, "app", p.App)
	}

	total := totalSteps
	req := pushward.UpdateRequest{
		State: "ENDED",
		Content: pushward.Content{
			Template:    "pipeline",
			Progress:    float64(currentStep) / float64(total),
			State:       "Sync Failed",
			Icon:        "xmark.circle.fill",
			Subtitle:    "ArgoCD \u00b7 " + p.App,
			AccentColor: "#FF3B30",
			CurrentStep: &currentStep,
			TotalSteps:  &total,
		},
	}

	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("ended activity", "slug", slug, "state", "Sync Failed")

	h.mu.Lock()
	if a, ok := h.apps[p.App]; ok {
		a.cleanupTimer = time.AfterFunc(h.config.PushWard.CleanupDelay, func() {
			h.cleanup(p.App)
		})
	}
	h.mu.Unlock()
}

func (h *Handler) handleHealthDegraded(ctx context.Context, p *argocd.WebhookPayload) {
	slug := slugForApp(p.App)

	h.mu.Lock()
	app, exists := h.apps[p.App]
	currentStep := 1
	if exists {
		currentStep = app.step
		if app.staleTimer != nil {
			app.staleTimer.Stop()
			app.staleTimer = nil
		}
	} else {
		app = &trackedApp{
			slug:     slug,
			appName:  p.App,
			revision: p.Revision,
			step:     1,
		}
		h.apps[p.App] = app
	}
	h.mu.Unlock()

	if !exists {
		if err := h.client.CreateActivity(ctx, slug, p.App, h.config.PushWard.Priority); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			delete(h.apps, p.App)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity (untracked health-degraded)", "slug", slug, "app", p.App)
	}

	total := totalSteps
	req := pushward.UpdateRequest{
		State: "ENDED",
		Content: pushward.Content{
			Template:    "pipeline",
			Progress:    float64(currentStep) / float64(total),
			State:       "Degraded",
			Icon:        "exclamationmark.triangle.fill",
			Subtitle:    "ArgoCD \u00b7 " + p.App,
			AccentColor: "#FF9500",
			CurrentStep: &currentStep,
			TotalSteps:  &total,
		},
	}

	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("ended activity", "slug", slug, "state", "Degraded")

	h.mu.Lock()
	if a, ok := h.apps[p.App]; ok {
		a.cleanupTimer = time.AfterFunc(h.config.PushWard.CleanupDelay, func() {
			h.cleanup(p.App)
		})
	}
	h.mu.Unlock()
}

func (h *Handler) forceEnd(appName string) {
	h.mu.Lock()
	app, ok := h.apps[appName]
	if !ok {
		h.mu.Unlock()
		return
	}
	slug := app.slug
	currentStep := app.step
	h.mu.Unlock()

	slog.Warn("force-ending stale sync", "slug", slug, "app", appName)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	total := totalSteps
	req := pushward.UpdateRequest{
		State: "ENDED",
		Content: pushward.Content{
			Template:    "pipeline",
			Progress:    float64(currentStep) / float64(total),
			State:       "Stale sync (auto-ended)",
			Icon:        "clock.badge.xmark",
			Subtitle:    "ArgoCD \u00b7 " + appName,
			AccentColor: "#8E8E93",
			CurrentStep: &currentStep,
			TotalSteps:  &total,
		},
	}
	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to force-end activity", "slug", slug, "error", err)
	}

	h.mu.Lock()
	if a, ok := h.apps[appName]; ok {
		a.cleanupTimer = time.AfterFunc(h.config.PushWard.CleanupDelay, func() {
			h.cleanup(appName)
		})
	}
	h.mu.Unlock()
}

func (h *Handler) cleanup(appName string) {
	h.mu.Lock()
	app, ok := h.apps[appName]
	if !ok {
		h.mu.Unlock()
		return
	}
	slug := app.slug
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := h.client.DeleteActivity(ctx, slug); err != nil {
		slog.Error("failed to delete activity", "slug", slug, "error", err)
		return
	}
	slog.Info("deleted activity", "slug", slug)

	h.mu.Lock()
	delete(h.apps, appName)
	h.mu.Unlock()
}
