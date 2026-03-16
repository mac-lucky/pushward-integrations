package proxmox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

var vmidRe = regexp.MustCompile(`(?:VM|CT)\s+(\d+)`)

type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.ProxmoxConfig
	ender   *lifecycle.Ender
}

func NewHandler(store state.Store, clients *client.Pool, cfg *config.ProxmoxConfig) *Handler {
	return &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "proxmox", lifecycle.EndConfig{
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

	var payload webhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		slog.Error("failed to decode proxmox webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	userKey := auth.KeyFromContext(r.Context())
	ctx := r.Context()

	switch payload.Type {
	case "vzdump":
		h.handleVzdump(ctx, userKey, &payload)
	case "replication":
		h.handleReplication(ctx, userKey, &payload)
	case "fencing":
		h.handleFencing(ctx, userKey, &payload)
	case "package-updates":
		h.handleUpdates(ctx, userKey, &payload)
	case "system":
		slog.Debug("proxmox system event, skipping", "hostname", payload.Hostname)
	default:
		slog.Debug("unknown proxmox event type", "type", payload.Type)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleVzdump(ctx context.Context, userKey string, p *webhookPayload) {
	vmid := "unknown"
	if m := vmidRe.FindStringSubmatch(p.Message); len(m) > 1 {
		vmid = m[1]
	}

	hash := sha256.Sum256([]byte(p.Hostname + vmid))
	slug := fmt.Sprintf("proxmox-backup-%x", hash[:4])
	mapKey := fmt.Sprintf("vzdump:%s:%s", p.Hostname, vmid)
	subtitle := fmt.Sprintf("Proxmox \u00b7 %s", truncateField(p.Hostname, 50))

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	msgLower := strings.ToLower(p.Message)
	titleLower := strings.ToLower(p.Title)

	if strings.Contains(msgLower, "starting") || strings.Contains(titleLower, "starting") {
		// Backup starting — create activity + ONGOING
		if err := cl.CreateActivity(ctx, slug, truncateField(p.Title, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create proxmox backup activity", "slug", slug, "error", err)
			return
		}

		step1 := intPtr(1)
		step2 := intPtr(2)
		content := pushward.Content{
			Template:    "pipeline",
			Progress:    0,
			State:       "Backing up...",
			Icon:        "externaldrive.fill.badge.timemachine",
			Subtitle:    subtitle,
			AccentColor: "#007AFF",
			CurrentStep: step1,
			TotalSteps:  step2,
		}

		req := pushward.UpdateRequest{State: "ONGOING", Content: content}
		if err := cl.UpdateActivity(ctx, slug, req); err != nil {
			slog.Error("failed to update proxmox backup activity", "slug", slug, "error", err)
			return
		}

		data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
		_ = h.store.Set(ctx, "proxmox", userKey, mapKey, "", data, h.config.StaleTimeout)

		slog.Info("proxmox backup started", "slug", slug, "vmid", vmid, "hostname", p.Hostname)
	} else if strings.Contains(msgLower, "finished successfully") {
		// Only end if we have a tracked start event
		existing, _ := h.store.Get(ctx, "proxmox", userKey, mapKey, "")
		if existing == nil {
			slog.Debug("proxmox backup complete but no tracked start, skipping", "slug", slug)
			return
		}

		step2 := intPtr(2)
		content := pushward.Content{
			Template:    "pipeline",
			Progress:    1.0,
			State:       "Backup Complete",
			Icon:        "checkmark.circle.fill",
			Subtitle:    subtitle,
			AccentColor: "#34C759",
			CurrentStep: step2,
			TotalSteps:  step2,
		}

		h.ender.ScheduleEnd(userKey, mapKey, slug, content)
		slog.Info("proxmox backup completed", "slug", slug, "vmid", vmid, "hostname", p.Hostname)
	} else if strings.Contains(msgLower, "failed") || strings.Contains(msgLower, "error") {
		existing, _ := h.store.Get(ctx, "proxmox", userKey, mapKey, "")
		if existing == nil {
			slog.Debug("proxmox backup failed but no tracked start, skipping", "slug", slug)
			return
		}

		step2 := intPtr(2)
		content := pushward.Content{
			Template:    "pipeline",
			Progress:    1.0,
			State:       "Backup Failed",
			Icon:        "xmark.circle.fill",
			Subtitle:    subtitle,
			AccentColor: "#FF3B30",
			CurrentStep: step2,
			TotalSteps:  step2,
		}

		h.ender.ScheduleEnd(userKey, mapKey, slug, content)
		slog.Info("proxmox backup failed", "slug", slug, "vmid", vmid, "hostname", p.Hostname)
	}
}

var replicationJobRe = regexp.MustCompile(`(?:job|Job)\s+([\d/]+)`)

func (h *Handler) handleReplication(ctx context.Context, userKey string, p *webhookPayload) {
	// Extract job ID from message — titles differ between start/finish phases.
	jobID := "unknown"
	if m := replicationJobRe.FindStringSubmatch(p.Message); len(m) > 1 {
		jobID = m[1]
	} else if m := replicationJobRe.FindStringSubmatch(p.Title); len(m) > 1 {
		jobID = m[1]
	}

	hash := sha256.Sum256([]byte(p.Hostname + jobID))
	slug := fmt.Sprintf("proxmox-repl-%x", hash[:4])
	mapKey := fmt.Sprintf("replication:%s:%s", p.Hostname, jobID)
	subtitle := fmt.Sprintf("Proxmox \u00b7 %s", truncateField(p.Hostname, 50))

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	msgLower := strings.ToLower(p.Message)
	titleLower := strings.ToLower(p.Title)

	if strings.Contains(msgLower, "starting") || strings.Contains(titleLower, "starting") {
		if err := cl.CreateActivity(ctx, slug, truncateField(p.Title, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create proxmox replication activity", "slug", slug, "error", err)
			return
		}

		step1 := intPtr(1)
		step2 := intPtr(2)
		content := pushward.Content{
			Template:    "pipeline",
			Progress:    0,
			State:       "Replicating...",
			Icon:        "arrow.triangle.2.circlepath",
			Subtitle:    subtitle,
			AccentColor: "#007AFF",
			CurrentStep: step1,
			TotalSteps:  step2,
		}

		req := pushward.UpdateRequest{State: "ONGOING", Content: content}
		if err := cl.UpdateActivity(ctx, slug, req); err != nil {
			slog.Error("failed to update proxmox replication activity", "slug", slug, "error", err)
			return
		}

		data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
		_ = h.store.Set(ctx, "proxmox", userKey, mapKey, "", data, h.config.StaleTimeout)

		slog.Info("proxmox replication started", "slug", slug, "hostname", p.Hostname)
	} else if strings.Contains(msgLower, "finished successfully") {
		// Only end if we have a tracked start event
		existing, _ := h.store.Get(ctx, "proxmox", userKey, mapKey, "")
		if existing == nil {
			slog.Debug("proxmox replication complete but no tracked start, skipping", "slug", slug)
			return
		}

		step2 := intPtr(2)
		content := pushward.Content{
			Template:    "pipeline",
			Progress:    1.0,
			State:       "Replication Complete",
			Icon:        "checkmark.circle.fill",
			Subtitle:    subtitle,
			AccentColor: "#34C759",
			CurrentStep: step2,
			TotalSteps:  step2,
		}

		h.ender.ScheduleEnd(userKey, mapKey, slug, content)
		slog.Info("proxmox replication completed", "slug", slug, "hostname", p.Hostname)
	} else if strings.Contains(msgLower, "failed") || strings.Contains(msgLower, "error") {
		existing, _ := h.store.Get(ctx, "proxmox", userKey, mapKey, "")
		if existing == nil {
			slog.Debug("proxmox replication failed but no tracked start, skipping", "slug", slug)
			return
		}

		step2 := intPtr(2)
		content := pushward.Content{
			Template:    "pipeline",
			Progress:    1.0,
			State:       "Replication Failed",
			Icon:        "xmark.circle.fill",
			Subtitle:    subtitle,
			AccentColor: "#FF3B30",
			CurrentStep: step2,
			TotalSteps:  step2,
		}

		h.ender.ScheduleEnd(userKey, mapKey, slug, content)
		slog.Info("proxmox replication failed", "slug", slug, "hostname", p.Hostname)
	}
}

func (h *Handler) handleFencing(ctx context.Context, userKey string, p *webhookPayload) {
	hash := sha256.Sum256([]byte(p.Hostname))
	slug := fmt.Sprintf("proxmox-fence-%x", hash[:4])
	mapKey := fmt.Sprintf("fencing:%s", p.Hostname)
	subtitle := fmt.Sprintf("Proxmox \u00b7 %s", truncateField(p.Hostname, 50))

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, truncateField(p.Title, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create proxmox fencing activity", "slug", slug, "error", err)
		return
	}

	content := pushward.Content{
		Template:    "alert",
		Progress:    1.0,
		State:       truncateField(p.Title, 100),
		Icon:        "exclamationmark.octagon.fill",
		Subtitle:    subtitle,
		AccentColor: "#FF3B30",
	}

	req := pushward.UpdateRequest{State: "ONGOING", Content: content}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update proxmox fencing activity", "slug", slug, "error", err)
		return
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	_ = h.store.Set(ctx, "proxmox", userKey, mapKey, "", data, h.config.StaleTimeout)

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	slog.Info("proxmox fencing event", "slug", slug, "hostname", p.Hostname)
}

func (h *Handler) handleUpdates(ctx context.Context, userKey string, p *webhookPayload) {
	hash := sha256.Sum256([]byte(p.Hostname))
	slug := fmt.Sprintf("proxmox-updates-%x", hash[:4])
	mapKey := fmt.Sprintf("updates:%s", p.Hostname)
	subtitle := fmt.Sprintf("Proxmox \u00b7 %s", truncateField(p.Hostname, 50))

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, truncateField(p.Title, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create proxmox updates activity", "slug", slug, "error", err)
		return
	}

	content := pushward.Content{
		Template:    "alert",
		Progress:    1.0,
		State:       truncateField(p.Title, 100),
		Icon:        "arrow.down.circle",
		Subtitle:    subtitle,
		AccentColor: "#007AFF",
	}

	req := pushward.UpdateRequest{State: "ONGOING", Content: content}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update proxmox updates activity", "slug", slug, "error", err)
		return
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	_ = h.store.Set(ctx, "proxmox", userKey, mapKey, "", data, h.config.StaleTimeout)

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	slog.Info("proxmox updates event", "slug", slug, "hostname", p.Hostname)
}

func intPtr(v int) *int { return &v }

func truncateField(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
