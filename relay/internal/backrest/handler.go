package backrest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overrides"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// Backrest's Hook.Condition enum, proto/v1/config.proto at v1.14.1.
const (
	condUnknown         = "CONDITION_UNKNOWN"
	condAnyError        = "CONDITION_ANY_ERROR"
	condSnapshotStart   = "CONDITION_SNAPSHOT_START"
	condSnapshotEnd     = "CONDITION_SNAPSHOT_END"
	condSnapshotError   = "CONDITION_SNAPSHOT_ERROR"
	condSnapshotWarning = "CONDITION_SNAPSHOT_WARNING"
	condSnapshotSuccess = "CONDITION_SNAPSHOT_SUCCESS"
	condSnapshotSkipped = "CONDITION_SNAPSHOT_SKIPPED"
	condPruneStart      = "CONDITION_PRUNE_START"
	condPruneError      = "CONDITION_PRUNE_ERROR"
	condPruneSuccess    = "CONDITION_PRUNE_SUCCESS"
	condCheckStart      = "CONDITION_CHECK_START"
	condCheckError      = "CONDITION_CHECK_ERROR"
	condCheckSuccess    = "CONDITION_CHECK_SUCCESS"
	condForgetStart     = "CONDITION_FORGET_START"
	condForgetError     = "CONDITION_FORGET_ERROR"
	condForgetSuccess   = "CONDITION_FORGET_SUCCESS"
)

const (
	stateBackingUp         = "Backing up..."
	stateComplete          = "Complete"
	stateCompleteWarnings  = "Complete (warnings)"
	stateFailed            = "Failed"
	statePruning           = "Pruning..."
	statePruned            = "Pruned"
	statePruneFailed       = "Prune Failed"
	stateChecking          = "Checking..."
	stateCheckPassed       = "Check Passed"
	stateCheckFailed       = "Check Failed"
	stateApplyingRetention = "Applying retention..."
	stateRetentionApplied  = "Retention applied"
	stateRetentionFailed   = "Retention failed"
	stateAlertError        = "Error"
	stateSnapshotSkipped   = "Snapshot Skipped"
)

// planSystem is Backrest's stand-in plan id on a repo-scoped task.
const planSystem = "_system_"

const (
	iconRunning = "arrow.triangle.2.circlepath"
	iconOK      = "checkmark.circle.fill"
	iconFail    = "xmark.circle.fill"
	iconWarn    = "exclamationmark.triangle.fill"
	iconInfo    = "info.circle.fill"
)

// eventKind is how a condition is surfaced. kindIgnore is the zero value so an
// unpopulated eventSpec degrades to a no-op rather than pushing a blank
// activity that never ends.
type eventKind int

const (
	kindIgnore eventKind = iota
	kindStart
	kindEnd
	kindAlert
)

type eventSpec struct {
	kind     eventKind
	state    string
	icon     string
	color    string
	severity string // kindAlert only
	// problem marks outcomes worth interrupting for: it drives both the
	// notification policy and the interruption level.
	problem bool
}

// Named because CONDITION_SNAPSHOT_END resolves to one of the two per request
// rather than carrying an outcome of its own.
var (
	specSnapshotOK   = eventSpec{kind: kindEnd, state: stateComplete, icon: iconOK, color: pushward.ColorGreen}
	specSnapshotFail = eventSpec{kind: kindEnd, state: stateFailed, icon: iconFail, color: pushward.ColorRed, problem: true}
)

// eventSpecs covers Backrest's Hook.Condition enum, minus two deliberate
// absences. CONDITION_SNAPSHOT_END is resolved per request by specFor, and a row
// here would silently disagree with it. CONDITION_UNKNOWN is Backrest's internal
// "no condition matched" sentinel, never delivered, so it falls through the same
// path as any unrecognised event.
var eventSpecs = map[string]eventSpec{
	condSnapshotStart:   {kind: kindStart, state: stateBackingUp, icon: iconRunning, color: pushward.ColorBlue},
	condSnapshotSuccess: specSnapshotOK,
	condSnapshotWarning: {kind: kindEnd, state: stateCompleteWarnings, icon: iconWarn, color: pushward.ColorOrange, problem: true},
	condSnapshotError:   specSnapshotFail,
	condSnapshotSkipped: {kind: kindAlert, state: stateSnapshotSkipped, icon: iconInfo, color: pushward.ColorBlue, severity: pushward.SeverityInfo},

	condPruneStart:   {kind: kindStart, state: statePruning, icon: iconRunning, color: pushward.ColorBlue},
	condPruneSuccess: {kind: kindEnd, state: statePruned, icon: iconOK, color: pushward.ColorGreen},
	condPruneError:   {kind: kindEnd, state: statePruneFailed, icon: iconFail, color: pushward.ColorRed, problem: true},

	condCheckStart:   {kind: kindStart, state: stateChecking, icon: iconRunning, color: pushward.ColorBlue},
	condCheckSuccess: {kind: kindEnd, state: stateCheckPassed, icon: iconOK, color: pushward.ColorGreen},
	condCheckError:   {kind: kindEnd, state: stateCheckFailed, icon: iconFail, color: pushward.ColorRed, problem: true},

	condForgetStart:   {kind: kindStart, state: stateApplyingRetention, icon: iconRunning, color: pushward.ColorBlue},
	condForgetSuccess: {kind: kindEnd, state: stateRetentionApplied, icon: iconOK, color: pushward.ColorGreen},
	condForgetError:   {kind: kindEnd, state: stateRetentionFailed, icon: iconFail, color: pushward.ColorRed, problem: true},

	// Fires for every task type, including setup-phase failures that never reach
	// a specific *_ERROR condition, so it gets its own alert slug rather than
	// ending an operation activity that may never have started.
	condAnyError: {kind: kindAlert, state: stateAlertError, icon: iconWarn, color: pushward.ColorRed, severity: pushward.SeverityCritical, problem: true},
}

var stepLabels = []string{"Running", "Done"}

type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.BackrestConfig
	ender   *lifecycle.Ender
}

// RegisterRoutes registers the Backrest webhook endpoint and returns the Handler
// so the caller can collect the Ender for graceful shutdown.
func RegisterRoutes(api huma.API, store state.Store, clients *client.Pool, cfg *config.BackrestConfig) *Handler {
	h := &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "backrest", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
	}
	humautil.RegisterWebhook(api, "/backrest", "post-backrest-webhook",
		"Receive Backrest backup webhook",
		"Processes Backrest backup lifecycle events (snapshot, prune, check, forget).",
		[]string{"Backrest"}, h.handleWebhook)
	return h
}

func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

// specFor maps a payload to how it should be surfaced. SNAPSHOT_END needs the
// payload, not just the condition name: Backrest fires completion with a list of
// conditions and delivers only the first one the hook subscribes to, END always
// last. So END reaches hooks subscribed to it alone, and there the error field
// is the only outcome signal there is.
func specFor(p *backrestPayload) (eventSpec, bool) {
	if p.Event == condSnapshotEnd {
		if p.Error != "" {
			return specSnapshotFail, true
		}
		return specSnapshotOK, true
	}
	spec, ok := eventSpecs[p.Event]
	return spec, ok
}

func (h *Handler) handleWebhook(ctx context.Context, input *struct {
	Body backrestPayload
},
) (*humautil.WebhookResponse, error) {
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	ctx = metrics.WithProvider(ctx, "backrest")
	payload := &input.Body

	spec, ok := specFor(payload)
	if !ok {
		slog.Debug("unknown backrest event", "event", payload.Event)
		return humautil.NewOK(), nil
	}

	var err error
	switch spec.kind {
	case kindStart:
		err = h.handleStart(ctx, userKey, log, payload, spec)
	case kindEnd:
		err = h.handleEnd(ctx, userKey, log, payload, spec)
	case kindAlert:
		err = h.handleAlert(ctx, userKey, log, payload, spec)
	case kindIgnore:
		slog.Debug("ignoring backrest event", "event", payload.Event)
	}

	if err != nil {
		return nil, humautil.UpstreamError(err)
	}
	return humautil.NewOK(), nil
}

func (h *Handler) slugAndKey(p *backrestPayload) (string, string) {
	slug := text.SlugHash("backrest", p.Plan+p.Repo, 4)
	mapKey := fmt.Sprintf("backrest:%s:%s", p.Plan, p.Repo)
	return slug, mapKey
}

func (h *Handler) subtitle(p *backrestPayload) string {
	parts := make([]string, 0, 3)
	parts = append(parts, "Backrest")
	if plan := p.planName(); plan != "" {
		parts = append(parts, text.TruncateHard(plan, 50))
	}
	if p.Repo != "" {
		parts = append(parts, text.TruncateHard(p.Repo, 50))
	}
	return strings.Join(parts, text.SepDot)
}

// summaryDetail renders whatever the template chose to send about the snapshot,
// e.g. "2.3 GB · 198 files · 45s". Empty when the template sends no stats.
func summaryDetail(p *backrestPayload) string {
	parts := make([]string, 0, 3)
	if p.DataAdded != nil && *p.DataAdded > 0 {
		parts = append(parts, text.FormatBytes(*p.DataAdded))
	}
	if n, ok := p.filesTouched(); ok && n > 0 {
		parts = append(parts, fmt.Sprintf("%d files", n))
	}
	if d, ok := p.elapsed(); ok {
		parts = append(parts, text.FormatDuration(d))
	}
	return strings.Join(parts, text.SepDot)
}

// scanDetail renders the totals restic scanned rather than stored. Too long for
// the activity's single state line, but they fit a notification body.
func scanDetail(p *backrestPayload) string {
	parts := make([]string, 0, 2)
	if p.TotalBytesProcessed != nil && *p.TotalBytesProcessed > 0 {
		parts = append(parts, text.FormatBytes(*p.TotalBytesProcessed)+" scanned")
	}
	if p.FilesUnmodified != nil && *p.FilesUnmodified > 0 {
		parts = append(parts, fmt.Sprintf("%d unchanged", *p.FilesUnmodified))
	}
	return strings.Join(parts, text.SepDot)
}

// endState composes the final state line. On a failure the error wins over the
// summary: it is the actionable half, and there is room for one.
func endState(spec eventSpec, p *backrestPayload) string {
	if spec.problem && p.Error != "" {
		return spec.state + ": " + text.TruncateHard(p.Error, 50)
	}
	if d := summaryDetail(p); d != "" {
		return spec.state + text.SepDot + d
	}
	return spec.state
}

// notify sends the push. hasActivity says whether an activity is open for slug,
// which is not the same question as whether this request may touch the activity
// surface: a suppressed outcome can still be closing one an earlier hook opened.
func (h *Handler) notify(ctx context.Context, userKey string, log *slog.Logger, p *backrestPayload, spec eventSpec, slug, stateText string, hasActivity bool) error {
	ov := overrides.FromContext(ctx)
	title := text.TruncateHard(p.planName(), 100)
	if title == "" {
		title = "Backrest"
	}
	level := pushward.LevelPassive
	if spec.problem {
		level = pushward.LevelTimeSensitive
	}
	body := stateText
	if scan := scanDetail(p); scan != "" {
		body += "\n" + scan
	}

	req := pushward.SendNotificationRequest{
		Title:      title,
		Subtitle:   h.subtitle(p),
		Body:       body,
		ThreadID:   "backrest",
		CollapseID: slug,
		Source:     "backrest",
		Level:      ov.LevelOr(level),
		Push:       pushward.BoolPtr(true),
		Metadata:   notificationMetadata(p),
	}
	// Deep-link only when an activity exists: a slug the tenant does not own is
	// rejected with 422 notification.activity_not_found, dropping the push too.
	if hasActivity {
		req.ActivitySlug = slug
	}
	return h.clients.SendNotification(ctx, userKey, log, req)
}

// notificationMetadata bounds every value: all of them are attacker-controlled.
func notificationMetadata(p *backrestPayload) map[string]string {
	md := map[string]string{"event": p.Event}
	if plan := p.planName(); plan != "" {
		md["plan"] = text.TruncateHard(plan, 100)
	}
	if p.Repo != "" {
		md["repo"] = text.TruncateHard(p.Repo, 100)
	}
	if p.SnapshotID != "" {
		md["snapshot_id"] = text.TruncateHard(p.SnapshotID, 100)
	}
	if p.Task != "" {
		md["task"] = text.TruncateHard(p.Task, 100)
	}
	return md
}

// rememberSlug records which activity is open for this plan and repo, so a later
// request can find it (see Ender.EndIfTracked) and so the ender can clear it.
func (h *Handler) rememberSlug(ctx context.Context, userKey string, log *slog.Logger, mapKey, slug string) {
	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "backrest", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		log.Warn("state store write failed", "error", err, "provider", "backrest", "slug", slug)
	}
}

func (h *Handler) createActivity(ctx context.Context, userKey string, log *slog.Logger, slug string, p *backrestPayload) (*pushward.Client, error) {
	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	name := text.TruncateHard(p.planName(), 100)
	if name == "" {
		name = "Backup"
	}

	if err := cl.CreateActivity(ctx, slug, name, overrides.FromContext(ctx).PriorityOr(h.config.Priority), endedTTL, staleTTL); err != nil {
		log.Error("failed to create backrest activity", "slug", slug, "error", err)
		return nil, err
	}
	return cl, nil
}

func (h *Handler) stepsContent(p *backrestPayload, spec eventSpec, stateText string, step int) pushward.Content {
	total := len(stepLabels)
	progress := 0.0
	if total > 1 {
		progress = float64(step-1) / float64(total-1)
	}
	return pushward.Content{
		Template:    pushward.TemplateSteps,
		Progress:    progress,
		State:       stateText,
		Icon:        spec.icon,
		Subtitle:    h.subtitle(p),
		AccentColor: spec.color,
		CurrentStep: &step,
		TotalSteps:  &total,
		StepLabels:  stepLabels,
	}
}

func (h *Handler) handleStart(ctx context.Context, userKey string, log *slog.Logger, p *backrestPayload, spec eventSpec) error {
	// Starts are not outcomes: never notify, even under channels=notification.
	if !overrides.FromContext(ctx).AllowsActivity() {
		return nil
	}
	slug, mapKey := h.slugAndKey(p)

	cl, err := h.createActivity(ctx, userKey, log, slug, p)
	if err != nil {
		return err
	}

	content := h.stepsContent(p, spec, spec.state, 1)
	if err := cl.UpdateActivity(ctx, slug, pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}); err != nil {
		log.Error("failed to update backrest activity", "slug", slug, "error", err)
		return err
	}

	h.rememberSlug(ctx, userKey, log, mapKey, slug)

	log.Info("backrest started", "slug", slug, "event", p.Event, "state", spec.state)
	return nil
}

func (h *Handler) handleEnd(ctx context.Context, userKey string, log *slog.Logger, p *backrestPayload, spec eventSpec) error {
	ov := overrides.FromContext(ctx)
	slug, mapKey := h.slugAndKey(p)
	stateText := endState(spec, p)
	content := h.stepsContent(p, spec, stateText, 2)

	if !ov.AllowsActivity() {
		// Backrest allows several hooks with different URLs, so a start can
		// arrive on /backrest while the outcome arrives with channels=notification.
		tracked := h.ender.EndIfTracked(ctx, log, userKey, mapKey, slug, content)
		if ov.NotifyFallback(spec.problem) {
			// The push is the only delivery left, so its failure is the
			// request's failure and the sender gets a 502 to retry on.
			return h.notify(ctx, userKey, log, p, spec, slug, stateText, tracked)
		}
		return nil
	}

	cl, err := h.createActivity(ctx, userKey, log, slug, p)
	if err != nil {
		return err
	}

	// Send the completion frame synchronously rather than waiting for the
	// ender's phase-1 an EndDelay later, or a finished backup sits on "Backing
	// up..." for several seconds (and a standalone SUCCESS/ERROR shows an empty
	// activity). Phase-1 then re-sends identical content, one redundant update.
	if err := cl.UpdateActivity(ctx, slug, pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}); err != nil {
		log.Error("failed to update backrest activity", "slug", slug, "error", err)
		return err
	}

	h.rememberSlug(ctx, userKey, log, mapKey, slug)

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)

	if ov.NotifyFallback(spec.problem) {
		// The activity already carries the outcome, so a failed push is logged
		// and swallowed rather than failing the webhook.
		_ = h.notify(ctx, userKey, log, p, spec, slug, stateText, true)
	}

	log.Info("backrest end scheduled", "slug", slug, "event", p.Event, "state", stateText)
	return nil
}

func (h *Handler) handleAlert(ctx context.Context, userKey string, log *slog.Logger, p *backrestPayload, spec eventSpec) error {
	ov := overrides.FromContext(ctx)
	// An alert keeps its own slug so a catch-all error never overwrites, or
	// prematurely ends, the operation activity it fired alongside.
	slug := text.SlugHash("backrest-alert", p.Plan+p.Repo+p.Event, 4)
	mapKey := fmt.Sprintf("backrest:alert:%s:%s:%s", p.Plan, p.Repo, p.Event)

	stateText := spec.state
	// When Backrest sends an error the raw message takes the state line, which
	// is where the condition would otherwise have been. Move the condition into
	// the badge so the card still says which hook fired, not just what it said.
	severityLabel := ""
	if p.Error != "" {
		stateText = text.TruncateHard(p.Error, 60)
		severityLabel = text.TruncateHard(spec.state, pushward.MaxSeverityLabelRunes)
	}

	content := pushward.Content{
		Template:      pushward.TemplateAlert,
		Progress:      1.0,
		State:         stateText,
		Icon:          spec.icon,
		Subtitle:      h.subtitle(p),
		AccentColor:   spec.color,
		Severity:      spec.severity,
		SeverityLabel: severityLabel,
	}

	// No EndIfTracked here, unlike handleEnd: an alert creates its activity and
	// schedules its end in the same request, so there is never one left open by
	// an earlier request for a later one to close.
	if !ov.AllowsActivity() {
		if ov.NotifyFallback(spec.problem) {
			return h.notify(ctx, userKey, log, p, spec, slug, stateText, false)
		}
		return nil
	}

	cl, err := h.createActivity(ctx, userKey, log, slug, p)
	if err != nil {
		return err
	}

	if err := cl.UpdateActivity(ctx, slug, pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}); err != nil {
		log.Error("failed to update backrest alert activity", "slug", slug, "error", err)
		return err
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)

	if ov.NotifyFallback(spec.problem) {
		_ = h.notify(ctx, userKey, log, p, spec, slug, stateText, true)
	}

	log.Info("backrest alert", "slug", slug, "event", p.Event, "state", stateText)
	return nil
}
