package starr

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// trackedSlug is stored in the state store as JSON.
type trackedSlug struct {
	Slug string `json:"slug"`
}

// Handler processes Radarr and Sonarr webhooks for multiple tenants.
type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.StarrConfig
	mu      sync.Mutex
	timers  map[string]*time.Timer // "userKey:mapKey" → end timer
}

var nonAlphanumeric = regexp.MustCompile(`[^a-z0-9]+`)

// NewHandler creates a new Starr webhook handler.
func NewHandler(store state.Store, clients *client.Pool, cfg *config.StarrConfig) *Handler {
	return &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		timers:  make(map[string]*time.Timer),
	}
}

// RadarrHandler returns an http.Handler for Radarr webhooks.
func (h *Handler) RadarrHandler() http.Handler {
	return http.HandlerFunc(h.handleRadarrWebhook)
}

// SonarrHandler returns an http.Handler for Sonarr webhooks.
func (h *Handler) SonarrHandler() http.Handler {
	return http.HandlerFunc(h.handleSonarrWebhook)
}

func slugForDownload(prefix, downloadID string) string {
	s := strings.ToLower(downloadID)
	s = nonAlphanumeric.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return prefix + s
}

// checkWebhookSecret validates the Basic Auth password against the configured
// webhook secret. In the relay, the Basic Auth username carries the hlk_ key
// (already extracted by the auth middleware), so only the password matters here.
func checkWebhookSecret(r *http.Request, secret string) bool {
	_, pass, ok := r.BasicAuth()
	return ok && subtle.ConstantTimeCompare([]byte(pass), []byte(secret)) == 1
}

func decodePayload(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, false
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		slog.Error("failed to decode webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return nil, false
	}
	return raw, true
}

// scheduleEnd schedules a two-phase end for an activity:
//   - Phase 1 (after EndDelay): ONGOING update with final content
//   - Phase 2 (EndDisplayTime later): ENDED with same content
func (h *Handler) scheduleEnd(userKey, mapKey, slug string, content pushward.Content) {
	timerKey := userKey + ":" + mapKey
	cl := h.clients.Get(userKey)
	endDelay := h.config.EndDelay
	displayTime := h.config.EndDisplayTime

	h.mu.Lock()
	if existing, ok := h.timers[timerKey]; ok {
		existing.Stop()
	}
	h.timers[timerKey] = time.AfterFunc(endDelay, func() {
		// Phase 1: ONGOING with final content
		ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
		ongoingReq := pushward.UpdateRequest{
			State:   "ONGOING",
			Content: content,
		}
		if err := cl.UpdateActivity(ctx1, slug, ongoingReq); err != nil {
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
		if err := cl.UpdateActivity(ctx2, slug, endedReq); err != nil {
			slog.Error("failed to end activity (end phase 2)", "slug", slug, "error", err)
		} else {
			slog.Info("ended activity", "slug", slug, "state", content.State)
		}

		// Clean up state store and timer
		delCtx, delCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer delCancel()
		_ = h.store.Delete(delCtx, "starr", userKey, mapKey, "")

		h.mu.Lock()
		delete(h.timers, timerKey)
		h.mu.Unlock()
	})
	h.mu.Unlock()
}

func truncate(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return string([]rune(s)[:maxLen])
	}
	return string([]rune(s)[:maxLen-3]) + "..."
}

// stopTimer cancels an existing end timer if present.
func (h *Handler) stopTimer(userKey, mapKey string) {
	timerKey := userKey + ":" + mapKey
	h.mu.Lock()
	if t, ok := h.timers[timerKey]; ok {
		t.Stop()
		delete(h.timers, timerKey)
	}
	h.mu.Unlock()
}

// getTrackedSlug retrieves a tracked download slug from the state store.
func (h *Handler) getTrackedSlug(ctx context.Context, userKey, mapKey string) (string, bool) {
	raw, err := h.store.Get(ctx, "starr", userKey, mapKey, "")
	if err != nil || raw == nil {
		return "", false
	}
	var ts trackedSlug
	if err := json.Unmarshal(raw, &ts); err != nil {
		return "", false
	}
	return ts.Slug, true
}

// setTrackedSlug stores a tracked download slug in the state store.
func (h *Handler) setTrackedSlug(ctx context.Context, userKey, mapKey, slug string) error {
	data, err := json.Marshal(trackedSlug{Slug: slug})
	if err != nil {
		return err
	}
	return h.store.Set(ctx, "starr", userKey, mapKey, "", data, 60*time.Minute)
}

// deleteTrackedSlug removes a tracked download from the state store.
func (h *Handler) deleteTrackedSlug(ctx context.Context, userKey, mapKey string) {
	_ = h.store.Delete(ctx, "starr", userKey, mapKey, "")
}
