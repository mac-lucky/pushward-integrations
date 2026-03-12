package handler

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

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/starr/internal/config"
)

type Handler struct {
	client    *pushward.Client
	config    *config.Config
	mu        sync.Mutex
	downloads map[string]*trackedDownload // keyed by "radarr:<id>" or "sonarr:<id>"
}

type trackedDownload struct {
	slug     string
	endTimer *time.Timer
}

var nonAlphanumeric = regexp.MustCompile(`[^a-z0-9]+`)

func New(client *pushward.Client, cfg *config.Config) *Handler {
	return &Handler{
		client:    client,
		config:    cfg,
		downloads: make(map[string]*trackedDownload),
	}
}

func slugForDownload(prefix, downloadID string) string {
	s := strings.ToLower(downloadID)
	s = nonAlphanumeric.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return prefix + s
}

func checkBasicAuth(r *http.Request, username, password string) bool {
	user, pass, ok := r.BasicAuth()
	return ok &&
		subtle.ConstantTimeCompare([]byte(user), []byte(username)) == 1 &&
		subtle.ConstantTimeCompare([]byte(pass), []byte(password)) == 1
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
func (h *Handler) scheduleEnd(mapKey, slug string, content pushward.Content) {
	h.mu.Lock()
	dl, ok := h.downloads[mapKey]
	if !ok {
		h.mu.Unlock()
		return
	}
	endDelay := h.config.PushWard.EndDelay
	displayTime := h.config.PushWard.EndDisplayTime
	dl.endTimer = time.AfterFunc(endDelay, func() {
		// Phase 1: ONGOING with final content
		ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
		ongoingReq := pushward.UpdateRequest{
			State:   "ONGOING",
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
			State:   "ENDED",
			Content: content,
		}
		if err := h.client.UpdateActivity(ctx2, slug, endedReq); err != nil {
			slog.Error("failed to end activity (end phase 2)", "slug", slug, "error", err)
		} else {
			slog.Info("ended activity", "slug", slug, "state", content.State)
		}

		h.mu.Lock()
		delete(h.downloads, mapKey)
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
