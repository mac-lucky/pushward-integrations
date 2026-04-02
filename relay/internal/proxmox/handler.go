package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
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

func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var payload webhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		slog.Error("failed to decode proxmox webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	userKey := auth.KeyFromContext(r.Context())
	log := slog.With("tenant", auth.KeyHash(userKey))
	ctx := r.Context()
	ctx = metrics.WithProvider(ctx, "proxmox")

	var apiErr error
	switch payload.Type {
	case "vzdump":
		apiErr = h.handleVzdump(ctx, userKey, log, &payload)
	case "replication":
		apiErr = h.handleReplication(ctx, userKey, log, &payload)
	case "fencing":
		apiErr = h.handleFencing(ctx, userKey, log, &payload)
	case "package-updates":
		apiErr = h.handleUpdates(ctx, userKey, log, &payload)
	case "system":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "proxmox"); err != nil {
			log.Error("test notification failed", "provider", "proxmox", "error", err)
		}
	default:
		slog.Debug("unknown proxmox event type", "type", payload.Type)
	}

	if apiErr != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleVzdump(ctx context.Context, userKey string, log *slog.Logger, p *webhookPayload) error {
	vmid := "unknown"
	if m := vmidRe.FindStringSubmatch(p.Message); len(m) > 1 {
		vmid = m[1]
	}

	slug := text.SlugHash("proxmox-backup", p.Hostname+vmid, 4)
	mapKey := fmt.Sprintf("vzdump:%s:%s", p.Hostname, vmid)
	subtitle := fmt.Sprintf("Proxmox \u00b7 %s", text.TruncateHard(p.Hostname, 50))

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	msgLower := strings.ToLower(p.Message)
	titleLower := strings.ToLower(p.Title)

	if strings.Contains(msgLower, "starting") || strings.Contains(titleLower, "starting") {
		// Backup starting — create activity + ONGOING
		if err := cl.CreateActivity(ctx, slug, text.TruncateHard(p.Title, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
			log.Error("failed to create proxmox backup activity", "slug", slug, "error", err)
			return err
		}

		// Write state before UpdateActivity so completion events can find it
		// even if the update below fails.
		data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
		if err := h.store.Set(ctx, "proxmox", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
			log.Warn("state store write failed", "error", err, "provider", "proxmox", "slug", slug)
		}

		step1 := pushward.IntPtr(1)
		step2 := pushward.IntPtr(2)
		content := pushward.Content{
			Template:    "steps",
			Progress:    0,
			State:       "Backing up...",
			Icon:        "externaldrive.fill.badge.timemachine",
			Subtitle:    subtitle,
			AccentColor: pushward.ColorBlue,
			CurrentStep: step1,
			TotalSteps:  step2,
		}

		req := pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}
		if err := cl.UpdateActivity(ctx, slug, req); err != nil {
			log.Error("failed to update proxmox backup activity", "slug", slug, "error", err)
			return err
		}

		log.Info("proxmox backup started", "slug", slug, "vmid", vmid, "hostname", p.Hostname)
	} else if strings.Contains(msgLower, "finished successfully") {
		// Only end if we have a tracked start event
		existing, _ := h.store.Get(ctx, "proxmox", userKey, mapKey, "")
		if existing == nil {
			slog.Debug("proxmox backup complete but no tracked start, skipping", "slug", slug)
			return nil
		}

		step2 := pushward.IntPtr(2)
		content := pushward.Content{
			Template:    "steps",
			Progress:    1.0,
			State:       "Backup Complete",
			Icon:        "checkmark.circle.fill",
			Subtitle:    subtitle,
			AccentColor: pushward.ColorGreen,
			CurrentStep: step2,
			TotalSteps:  step2,
		}

		h.ender.ScheduleEnd(userKey, mapKey, slug, content)
		log.Info("proxmox backup completed", "slug", slug, "vmid", vmid, "hostname", p.Hostname)
	} else if strings.Contains(msgLower, "failed") || strings.Contains(msgLower, "error") {
		existing, _ := h.store.Get(ctx, "proxmox", userKey, mapKey, "")
		if existing == nil {
			slog.Debug("proxmox backup failed but no tracked start, skipping", "slug", slug)
			return nil
		}

		step2 := pushward.IntPtr(2)
		content := pushward.Content{
			Template:    "steps",
			Progress:    1.0,
			State:       "Backup Failed",
			Icon:        "xmark.circle.fill",
			Subtitle:    subtitle,
			AccentColor: pushward.ColorRed,
			CurrentStep: step2,
			TotalSteps:  step2,
		}

		h.ender.ScheduleEnd(userKey, mapKey, slug, content)
		log.Info("proxmox backup failed", "slug", slug, "vmid", vmid, "hostname", p.Hostname)
	}
	return nil
}

var replicationJobRe = regexp.MustCompile(`(?:job|Job)\s+([\d/]+)`)

func (h *Handler) handleReplication(ctx context.Context, userKey string, log *slog.Logger, p *webhookPayload) error {
	// Extract job ID from message — titles differ between start/finish phases.
	jobID := "unknown"
	if m := replicationJobRe.FindStringSubmatch(p.Message); len(m) > 1 {
		jobID = m[1]
	} else if m := replicationJobRe.FindStringSubmatch(p.Title); len(m) > 1 {
		jobID = m[1]
	}

	slug := text.SlugHash("proxmox-repl", p.Hostname+jobID, 4)
	mapKey := fmt.Sprintf("replication:%s:%s", p.Hostname, jobID)
	subtitle := fmt.Sprintf("Proxmox \u00b7 %s", text.TruncateHard(p.Hostname, 50))

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	msgLower := strings.ToLower(p.Message)
	titleLower := strings.ToLower(p.Title)

	if strings.Contains(msgLower, "starting") || strings.Contains(titleLower, "starting") {
		if err := cl.CreateActivity(ctx, slug, text.TruncateHard(p.Title, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
			log.Error("failed to create proxmox replication activity", "slug", slug, "error", err)
			return err
		}

		data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
		if err := h.store.Set(ctx, "proxmox", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
			log.Warn("state store write failed", "error", err, "provider", "proxmox", "slug", slug)
		}

		step1 := pushward.IntPtr(1)
		step2 := pushward.IntPtr(2)
		content := pushward.Content{
			Template:    "steps",
			Progress:    0,
			State:       "Replicating...",
			Icon:        "arrow.triangle.2.circlepath",
			Subtitle:    subtitle,
			AccentColor: pushward.ColorBlue,
			CurrentStep: step1,
			TotalSteps:  step2,
		}

		req := pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}
		if err := cl.UpdateActivity(ctx, slug, req); err != nil {
			log.Error("failed to update proxmox replication activity", "slug", slug, "error", err)
			return err
		}

		log.Info("proxmox replication started", "slug", slug, "hostname", p.Hostname)
	} else if strings.Contains(msgLower, "finished successfully") {
		// Only end if we have a tracked start event
		existing, _ := h.store.Get(ctx, "proxmox", userKey, mapKey, "")
		if existing == nil {
			slog.Debug("proxmox replication complete but no tracked start, skipping", "slug", slug)
			return nil
		}

		step2 := pushward.IntPtr(2)
		content := pushward.Content{
			Template:    "steps",
			Progress:    1.0,
			State:       "Replication Complete",
			Icon:        "checkmark.circle.fill",
			Subtitle:    subtitle,
			AccentColor: pushward.ColorGreen,
			CurrentStep: step2,
			TotalSteps:  step2,
		}

		h.ender.ScheduleEnd(userKey, mapKey, slug, content)
		log.Info("proxmox replication completed", "slug", slug, "hostname", p.Hostname)
	} else if strings.Contains(msgLower, "failed") || strings.Contains(msgLower, "error") {
		existing, _ := h.store.Get(ctx, "proxmox", userKey, mapKey, "")
		if existing == nil {
			slog.Debug("proxmox replication failed but no tracked start, skipping", "slug", slug)
			return nil
		}

		step2 := pushward.IntPtr(2)
		content := pushward.Content{
			Template:    "steps",
			Progress:    1.0,
			State:       "Replication Failed",
			Icon:        "xmark.circle.fill",
			Subtitle:    subtitle,
			AccentColor: pushward.ColorRed,
			CurrentStep: step2,
			TotalSteps:  step2,
		}

		h.ender.ScheduleEnd(userKey, mapKey, slug, content)
		log.Info("proxmox replication failed", "slug", slug, "hostname", p.Hostname)
	}
	return nil
}

func (h *Handler) handleFencing(ctx context.Context, userKey string, log *slog.Logger, p *webhookPayload) error {
	slug := text.SlugHash("proxmox-fence", p.Hostname, 4)
	mapKey := fmt.Sprintf("fencing:%s", p.Hostname)
	subtitle := fmt.Sprintf("Proxmox \u00b7 %s", text.TruncateHard(p.Hostname, 50))

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, text.TruncateHard(p.Title, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
		log.Error("failed to create proxmox fencing activity", "slug", slug, "error", err)
		return err
	}

	content := pushward.Content{
		Template:    "alert",
		Progress:    1.0,
		State:       text.TruncateHard(p.Title, 100),
		Icon:        "exclamationmark.octagon.fill",
		Subtitle:    subtitle,
		AccentColor: pushward.ColorRed,
		Severity:    "critical",
	}

	req := pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update proxmox fencing activity", "slug", slug, "error", err)
		return err
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "proxmox", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		log.Warn("state store write failed", "error", err, "provider", "proxmox", "slug", slug)
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	log.Info("proxmox fencing event", "slug", slug, "hostname", p.Hostname)
	return nil
}

func (h *Handler) handleUpdates(ctx context.Context, userKey string, log *slog.Logger, p *webhookPayload) error {
	slug := text.SlugHash("proxmox-updates", p.Hostname, 4)
	mapKey := fmt.Sprintf("updates:%s", p.Hostname)
	subtitle := fmt.Sprintf("Proxmox \u00b7 %s", text.TruncateHard(p.Hostname, 50))

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, text.TruncateHard(p.Title, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
		log.Error("failed to create proxmox updates activity", "slug", slug, "error", err)
		return err
	}

	content := pushward.Content{
		Template:    "alert",
		Progress:    1.0,
		State:       text.TruncateHard(p.Title, 100),
		Icon:        "arrow.down.circle",
		Subtitle:    subtitle,
		AccentColor: pushward.ColorBlue,
		Severity:    "info",
	}

	req := pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update proxmox updates activity", "slug", slug, "error", err)
		return err
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "proxmox", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		log.Warn("state store write failed", "error", err, "provider", "proxmox", "slug", slug)
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	log.Info("proxmox updates event", "slug", slug, "hostname", p.Hostname)
	return nil
}
