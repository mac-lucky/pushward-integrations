package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
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

// RegisterRoutes registers the Proxmox webhook endpoint and returns the Handler.
func RegisterRoutes(api huma.API, store state.Store, clients *client.Pool, cfg *config.ProxmoxConfig) *Handler {
	h := &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "proxmox", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
	}
	humautil.RegisterWebhook(api, "/proxmox", "post-proxmox-webhook",
		"Receive Proxmox notification webhook",
		"Processes Proxmox infrastructure events (vzdump, replication, fencing, package-updates).",
		[]string{"Proxmox"}, h.handleWebhook)
	return h
}

func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

func (h *Handler) handleWebhook(ctx context.Context, input *struct {
	Body proxmoxPayload
},
) (*humautil.WebhookResponse, error) {
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	ctx = metrics.WithProvider(ctx, "proxmox")
	payload := &input.Body

	var apiErr error
	switch payload.Type {
	case "vzdump":
		apiErr = h.handleVzdump(ctx, userKey, log, payload)
	case "replication":
		apiErr = h.handleReplication(ctx, userKey, log, payload)
	case "fencing":
		apiErr = h.handleFencing(ctx, userKey, log, payload)
	case "package-updates":
		apiErr = h.handleUpdates(ctx, userKey, log, payload)
	case "system-mail":
		apiErr = h.handleSystemMail(ctx, userKey, log, payload)
	case "", "test", "system":
		// Proxmox's "Test" button (Datacenter > Notifications) fires a
		// notification with an empty type metadata field. Treat that, and the
		// explicit "test"/"system" aliases, as a self-test so users can verify
		// the integration end to end.
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "proxmox"); err != nil {
			log.Error("test notification failed", "provider", "proxmox", "error", err)
		}
	default:
		log.Debug("unknown proxmox event type", "type", payload.Type)
	}

	if apiErr != nil {
		return nil, huma.Error502BadGateway("upstream API error")
	}
	return humautil.NewOK(), nil
}

func (h *Handler) handleVzdump(ctx context.Context, userKey string, log *slog.Logger, p *proxmoxPayload) error {
	vmid := "unknown"
	if m := vmidRe.FindStringSubmatch(p.Message); len(m) > 1 {
		vmid = m[1]
	}

	// Separate the hash inputs (matching the mapKey's ':' delimiter) so e.g.
	// (node1,23) and (node12,3) don't both hash "node123" to one slug.
	slug := text.SlugHash("proxmox-backup", p.Hostname+":"+vmid, 4)
	mapKey := fmt.Sprintf("vzdump:%s:%s", p.Hostname, vmid)
	subtitle := fmt.Sprintf("Proxmox \u00b7 %s", text.TruncateHard(p.Hostname, 50))

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	msgLower := strings.ToLower(p.Message)
	titleLower := strings.ToLower(p.Title)

	if strings.Contains(msgLower, "starting") || strings.Contains(titleLower, "starting") {
		// Cancel any pending two-phase end from a prior cycle on this key so a
		// new backup within the end window isn't clobbered and prematurely ended.
		h.ender.StopTimer(userKey, mapKey)
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
		// Only end if we have a tracked start event. Surface a store error as
		// 502 so the completion event is retried — otherwise a transient DB blip
		// is indistinguishable from "no start" and the activity lingers.
		existing, err := h.store.Get(ctx, "proxmox", userKey, mapKey, "")
		if err != nil {
			log.Error("failed to read proxmox backup state", "slug", slug, "error", err)
			return err
		}
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
		existing, err := h.store.Get(ctx, "proxmox", userKey, mapKey, "")
		if err != nil {
			log.Error("failed to read proxmox backup state", "slug", slug, "error", err)
			return err
		}
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

// Proxmox replication job IDs are <vmid>-<index> (e.g. 100-0) and may be
// single-quoted in messages ("Replication job '100-0' ..."). Capture the full
// id including the hyphen; tolerate quotes and the rarer slash form.
var replicationJobRe = regexp.MustCompile(`(?:job|Job)\s+'?([\d/-]+)'?`)

func (h *Handler) handleReplication(ctx context.Context, userKey string, log *slog.Logger, p *proxmoxPayload) error {
	// Extract job ID from message — titles differ between start/finish phases.
	jobID := "unknown"
	if m := replicationJobRe.FindStringSubmatch(p.Message); len(m) > 1 {
		jobID = m[1]
	} else if m := replicationJobRe.FindStringSubmatch(p.Title); len(m) > 1 {
		jobID = m[1]
	}

	slug := text.SlugHash("proxmox-repl", p.Hostname+":"+jobID, 4)
	mapKey := fmt.Sprintf("replication:%s:%s", p.Hostname, jobID)
	subtitle := fmt.Sprintf("Proxmox \u00b7 %s", text.TruncateHard(p.Hostname, 50))

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	msgLower := strings.ToLower(p.Message)
	titleLower := strings.ToLower(p.Title)

	if strings.Contains(msgLower, "starting") || strings.Contains(titleLower, "starting") {
		// Cancel any pending end from a prior cycle so a new replication within
		// the end window isn't clobbered and prematurely ended.
		h.ender.StopTimer(userKey, mapKey)
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
		// Only end if we have a tracked start event; surface store errors as 502.
		existing, err := h.store.Get(ctx, "proxmox", userKey, mapKey, "")
		if err != nil {
			log.Error("failed to read proxmox replication state", "slug", slug, "error", err)
			return err
		}
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
		existing, err := h.store.Get(ctx, "proxmox", userKey, mapKey, "")
		if err != nil {
			log.Error("failed to read proxmox replication state", "slug", slug, "error", err)
			return err
		}
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

func (h *Handler) handleFencing(ctx context.Context, userKey string, log *slog.Logger, p *proxmoxPayload) error {
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

func (h *Handler) handleUpdates(ctx context.Context, userKey string, log *slog.Logger, p *proxmoxPayload) error {
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

// handleSystemMail surfaces Proxmox's catch-all "system-mail" notifications
// (certificate renewals, ZFS/SMART errors, ad-hoc system events) as an alert
// activity. It collapses to one activity per host (like handleFencing /
// handleUpdates) rather than per title: system-mail is the highest-volume,
// least-structured type, and a per-title slug would let a routine maintenance
// mail storm exceed the server's small concurrent-activity budget and evict
// in-progress backups. There is no completion event to correlate, so unlike
// vzdump/replication it persists no state (the ender's cleanup Delete is a
// harmless no-op).
func (h *Handler) handleSystemMail(ctx context.Context, userKey string, log *slog.Logger, p *proxmoxPayload) error {
	hostname := p.Hostname
	if hostname == "" {
		hostname = "system"
	}
	// Proxmox forwards mailed events here; a missing subject yields an empty
	// title, which the server rejects (name is required). Fall back so the
	// alert still fires.
	title := p.Title
	if title == "" {
		title = "Proxmox system event"
	}
	slug := text.SlugHash("proxmox-system", hostname, 4)
	mapKey := fmt.Sprintf("system-mail:%s", hostname)
	subtitle := fmt.Sprintf("Proxmox \u00b7 %s", text.TruncateHard(hostname, 50))

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, text.TruncateHard(title, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
		log.Error("failed to create proxmox system activity", "slug", slug, "error", err)
		return err
	}

	// Map the Proxmox severity (error/warning/notice/info) onto PushWard's
	// vocabulary, then reuse SeverityColor for the accent.
	severity := pushward.SeverityInfo
	switch strings.ToLower(p.Severity) {
	case "error":
		severity = pushward.SeverityCritical
	case "warning":
		severity = pushward.SeverityWarning
	}
	icon := "bell.fill"
	if severity != pushward.SeverityInfo {
		icon = "exclamationmark.triangle.fill"
	}

	content := pushward.Content{
		Template:    "alert",
		Progress:    1.0,
		State:       text.TruncateHard(title, 100),
		Icon:        icon,
		Subtitle:    subtitle,
		AccentColor: pushward.SeverityColor(severity),
		Severity:    severity,
	}

	req := pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update proxmox system activity", "slug", slug, "error", err)
		return err
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	log.Info("proxmox system event", "slug", slug, "hostname", hostname)
	return nil
}
