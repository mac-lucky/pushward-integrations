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
	"github.com/mac-lucky/pushward-integrations/relay/internal/overrides"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

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
		apiErr = selftest.SendTest(ctx, h.clients.Get(userKey), log, "proxmox")
	default:
		log.Debug("unknown proxmox event type", "type", payload.Type)
	}

	if apiErr != nil {
		return nil, humautil.UpstreamError(apiErr)
	}
	return humautil.NewOK(), nil
}

// jobKind is what differs between the two job notifications.
type jobKind struct {
	noun        string // for logs
	slugPrefix  string
	mapPrefix   string
	running     string
	runningIcon string
	done        string
	failed      string
}

var (
	backupJob = jobKind{
		noun: "backup", slugPrefix: "proxmox-backup", mapPrefix: "vzdump",
		running: "Backing up...", runningIcon: "externaldrive.fill.badge.timemachine",
		done: "Backup Complete", failed: "Backup Failed",
	}
	replicationJob = jobKind{
		noun: "replication", slugPrefix: "proxmox-repl", mapPrefix: "replication",
		running: "Replicating...", runningIcon: "arrow.triangle.2.circlepath",
		done: "Replication Complete", failed: "Replication Failed",
	}
)

// handleVzdump keys the card on the job, not a guest: one notification covers
// every guest the job backed up. PVE sets job-id only for scheduled jobs, so a
// manual backup falls back to the node, where vzdump runs one job at a time.
func (h *Handler) handleVzdump(ctx context.Context, userKey string, log *slog.Logger, p *proxmoxPayload) error {
	return h.handleJob(ctx, userKey, log, p, backupJob, p.JobID)
}

// Proxmox replication job IDs are <vmid>-<index> (e.g. 100-0) and may be
// single-quoted ("Replication job '100-0' ...", "Replication Job: '100-0'
// failed"). Capture the full id including the hyphen; tolerate quotes and the
// rarer slash form. Only used when the body template sends no job_id.
var replicationJobRe = regexp.MustCompile(`(?:job|Job):?\s+'?([\d/-]+)'?`)

func (h *Handler) handleReplication(ctx context.Context, userKey string, log *slog.Logger, p *proxmoxPayload) error {
	jobID := p.JobID
	if jobID == "" {
		jobID = "unknown"
		if m := replicationJobRe.FindStringSubmatch(p.Message); len(m) > 1 {
			jobID = m[1]
		} else if m := replicationJobRe.FindStringSubmatch(p.Title); len(m) > 1 {
			jobID = m[1]
		}
	}
	return h.handleJob(ctx, userKey, log, p, replicationJob, jobID)
}

// handleJob turns a vzdump or replication notification into a steps card.
// PVE notifies once, when the job has ended: a backup with severity info or
// error, a replication only when it failed. So the outcome is read first, from
// the severity and the title, and the card is created and ended in the same
// request. The message is no signal: the vzdump body carries the job log, which
// always says "Starting Backup of VM ...". Only a title with "start" in it,
// which PVE never sends but a hook script might, opens a running card for a
// later outcome on the same node and job ID to end.
func (h *Handler) handleJob(ctx context.Context, userKey string, log *slog.Logger, p *proxmoxPayload, kind jobKind, id string) error {
	ov := overrides.FromContext(ctx)
	// Proxmox is Live-Activity-only with no notification path, so
	// channels=notification has nothing to fall back to.
	if !ov.AllowsActivity() {
		return nil
	}
	// Separate the hash inputs (matching the mapKey's ':' delimiter) so e.g.
	// (node1,23) and (node12,3) don't both hash "node123" to one slug.
	slug := text.SlugHash(kind.slugPrefix, p.Hostname+":"+id, 4)
	mapKey := fmt.Sprintf("%s:%s:%s", kind.mapPrefix, p.Hostname, id)

	title := strings.ToLower(p.Title)
	// The severity is PVE's outcome signal. The title also names the node
	// ("pve-failover"), so it decides only when a body sends no severity.
	severity := strings.TrimSpace(p.Severity)
	failed := strings.EqualFold(severity, "error") || (severity == "" && strings.Contains(title, "fail"))
	starting := !failed && !strings.Contains(title, "success") && strings.Contains(title, "start")

	content := pushward.Content{
		Template:    pushward.TemplateSteps,
		Progress:    1.0,
		State:       kind.done,
		Icon:        "checkmark.circle.fill",
		Subtitle:    "Proxmox" + text.SepDot + text.TruncateHard(p.Hostname, 50),
		AccentColor: pushward.ColorGreen,
		CurrentStep: pushward.IntPtr(2),
		TotalSteps:  pushward.IntPtr(2),
	}
	switch {
	case starting:
		content.Progress, content.State, content.Icon = 0, kind.running, kind.runningIcon
		content.AccentColor, content.CurrentStep = pushward.ColorBlue, pushward.IntPtr(1)
	case failed:
		content.State, content.Icon, content.AccentColor = kind.failed, "xmark.circle.fill", pushward.ColorRed
	}

	// Cancel a pending end from an earlier run on this key, so it cannot end
	// the card this request opens.
	h.ender.StopTimer(userKey, mapKey)
	// CreateActivity is an upsert, so an outcome opens the card whether or not
	// a start did, and its frame goes out now rather than at the ender's phase 1.
	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())
	if err := cl.CreateActivity(ctx, slug, text.TruncateHard(p.Title, 100), ov.PriorityOr(h.config.Priority), endedTTL, staleTTL); err != nil {
		log.Error("failed to create proxmox activity", "job", kind.noun, "slug", slug, "error", err)
		return err
	}
	if err := cl.UpdateActivity(ctx, slug, pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}); err != nil {
		log.Error("failed to update proxmox activity", "job", kind.noun, "slug", slug, "error", err)
		return err
	}
	if !starting {
		h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	}
	log.Info("proxmox job event", "job", kind.noun, "state", content.State, "slug", slug, "hostname", p.Hostname)
	return nil
}

func (h *Handler) handleFencing(ctx context.Context, userKey string, log *slog.Logger, p *proxmoxPayload) error {
	ov := overrides.FromContext(ctx)
	if !ov.AllowsActivity() {
		return nil
	}
	slug := text.SlugHash("proxmox-fence", p.Hostname, 4)
	mapKey := fmt.Sprintf("fencing:%s", p.Hostname)
	subtitle := fmt.Sprintf("Proxmox · %s", text.TruncateHard(p.Hostname, 50))

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, text.TruncateHard(p.Title, 100), ov.PriorityOr(h.config.Priority), endedTTL, staleTTL); err != nil {
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
		// The badge names the event class; the state line is the raw title.
		SeverityLabel: pushward.SeverityLabel("Fencing"),
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
	ov := overrides.FromContext(ctx)
	if !ov.AllowsActivity() {
		return nil
	}
	slug := text.SlugHash("proxmox-updates", p.Hostname, 4)
	mapKey := fmt.Sprintf("updates:%s", p.Hostname)
	subtitle := fmt.Sprintf("Proxmox · %s", text.TruncateHard(p.Hostname, 50))

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, text.TruncateHard(p.Title, 100), ov.PriorityOr(h.config.Priority), endedTTL, staleTTL); err != nil {
		log.Error("failed to create proxmox updates activity", "slug", slug, "error", err)
		return err
	}

	content := pushward.Content{
		Template:      "alert",
		Progress:      1.0,
		State:         text.TruncateHard(p.Title, 100),
		Icon:          "arrow.down.circle",
		Subtitle:      subtitle,
		AccentColor:   pushward.ColorBlue,
		Severity:      "info",
		SeverityLabel: pushward.SeverityLabel("Updates"),
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
// in-progress backups. There is no completion event to correlate, so it
// persists no state (the ender's cleanup Delete is a harmless no-op).
func (h *Handler) handleSystemMail(ctx context.Context, userKey string, log *slog.Logger, p *proxmoxPayload) error {
	ov := overrides.FromContext(ctx)
	if !ov.AllowsActivity() {
		return nil
	}
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
	subtitle := fmt.Sprintf("Proxmox · %s", text.TruncateHard(hostname, 50))

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, text.TruncateHard(title, 100), ov.PriorityOr(h.config.Priority), endedTTL, staleTTL); err != nil {
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
	// Proxmox has four levels and PushWard has three, so the switch above folds
	// notice into info. The badge carries the original word, which is the only
	// place the distinction survives.
	severityLabel := pushward.SeverityLabel(text.Capitalize(strings.ToLower(strings.TrimSpace(p.Severity))))

	content := pushward.Content{
		Template:      "alert",
		Progress:      1.0,
		State:         text.TruncateHard(title, 100),
		Icon:          icon,
		Subtitle:      subtitle,
		AccentColor:   pushward.SeverityColor(severity),
		Severity:      severity,
		SeverityLabel: severityLabel,
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
