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

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

var filenameRe = regexp.MustCompile(`(?:Successfully processed|Failed to process): (.+)`)

// Handler processes Unmanic Apprise webhooks for multiple tenants.
type Handler struct {
	clients *client.Pool
	config  *config.UnmanicConfig
	ender   *lifecycle.Ender
}

// NewHandler creates a new Unmanic webhook handler.
func NewHandler(clients *client.Pool, cfg *config.UnmanicConfig) *Handler {
	return &Handler{
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, nil, "unmanic", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
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

	h.ender.ScheduleEnd(userKey, slug, slug, content)
	slog.Info("unmanic activity", "slug", slug, "type", p.Type, "filename", filename)
}
