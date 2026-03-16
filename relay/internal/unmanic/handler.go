package unmanic

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

var filenameRe = regexp.MustCompile(`(?:Successfully processed|Failed to process): (.+)`)

type Handler struct {
	clients *client.Pool
	config  *config.UnmanicConfig
	mu      sync.Mutex
	timers  map[string]*time.Timer // "userKey:slug" → end timer
}

func NewHandler(clients *client.Pool, cfg *config.UnmanicConfig) *Handler {
	return &Handler{
		clients: clients,
		config:  cfg,
		timers:  make(map[string]*time.Timer),
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userKey := auth.KeyFromContext(r.Context())

	var p apprisePayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		slog.Error("failed to decode unmanic payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	switch p.Type {
	case "success":
		h.handleResult(r.Context(), userKey, p, true)
	case "failure":
		h.handleResult(r.Context(), userKey, p, false)
	case "info":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(r.Context(), cl, "unmanic"); err != nil {
			slog.Error("test notification failed", "provider", "unmanic", "error", err)
		}
	default:
		slog.Debug("unmanic unknown type", "type", p.Type)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func parseFilename(message string) string {
	m := filenameRe.FindStringSubmatch(message)
	if len(m) < 2 {
		return "unknown"
	}
	return path.Base(m[1])
}

func slugForFile(filename string) string {
	h := sha256.Sum256([]byte(filename))
	return fmt.Sprintf("unmanic-%x", h[:4])
}

func (h *Handler) handleResult(ctx context.Context, userKey string, p apprisePayload, success bool) {
	filename := parseFilename(p.Message)
	slug := slugForFile(filename)

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, filename, h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create unmanic activity", "slug", slug, "error", err)
		return
	}

	var content pushward.Content
	if success {
		content = pushward.Content{
			Template:    "generic",
			Progress:    1.0,
			State:       "Complete",
			Icon:        "checkmark.circle.fill",
			Subtitle:    "Unmanic \u00b7 " + filename,
			AccentColor: "#34C759",
		}
	} else {
		content = pushward.Content{
			Template:    "generic",
			Progress:    1.0,
			State:       "Failed",
			Icon:        "xmark.circle.fill",
			Subtitle:    "Unmanic \u00b7 " + filename,
			AccentColor: "#FF3B30",
		}
	}

	h.scheduleEnd(userKey, slug, content)
	slog.Info("unmanic activity", "slug", slug, "type", p.Type, "filename", filename)
}

// scheduleEnd schedules a two-phase end for an activity:
//   - Phase 1 (after EndDelay): ONGOING update with final content
//   - Phase 2 (EndDisplayTime later): ENDED with same content
func (h *Handler) scheduleEnd(userKey, slug string, content pushward.Content) {
	timerKey := userKey + ":" + slug
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
			slog.Error("failed to update unmanic activity (end phase 1)", "slug", slug, "error", err)
		} else {
			slog.Info("updated unmanic activity", "slug", slug, "state", content.State)
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
			slog.Error("failed to end unmanic activity (end phase 2)", "slug", slug, "error", err)
		} else {
			slog.Info("ended unmanic activity", "slug", slug, "state", content.State)
		}

		h.mu.Lock()
		delete(h.timers, timerKey)
		h.mu.Unlock()
	})
	h.mu.Unlock()
}
