package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
	"github.com/mac-lucky/pushward-integrations/unraid/internal/config"
	"github.com/mac-lucky/pushward-integrations/unraid/internal/graphql"
)

// timerPair holds both phase-1 and phase-2 timers so scheduleEnd can cancel both.
type timerPair struct {
	phase1 *time.Timer
	phase2 *time.Timer
}

// arrayPollInterval is how often the tracker polls `query { array }`
// because `arraySubscription` is broken on Unraid's side.
const arrayPollInterval = 10 * time.Second

// Tracker monitors Unraid array status and notifications. Parity checks and
// array state transitions are rendered as PushWard Live Activities; every
// Unraid notification is forwarded to the PushWard notification API.
type Tracker struct {
	cfg *config.Config
	gql *graphql.Client
	pw  *pushward.Client

	mu sync.Mutex
	// parityActive flips true only after the seed PATCH has landed, so the
	// debounced tick path can assume a server-side Content baseline exists.
	parityActive   bool
	parityLastSent time.Time
	arrayState     graphql.ArrayState
	timers         map[string]*timerPair
}

// New creates a new Tracker.
func New(cfg *config.Config, gql *graphql.Client, pw *pushward.Client) *Tracker {
	return &Tracker{
		cfg:    cfg,
		gql:    gql,
		pw:     pw,
		timers: make(map[string]*timerPair),
	}
}

// Run polls array state and subscribes to notifications until ctx is
// cancelled. Array uses polling because Unraid's arraySubscription is
// broken server-side.
func (t *Tracker) Run(ctx context.Context) error {
	notifCh := make(chan graphql.Notification, 10)

	go func() {
		if err := t.gql.SubscribeNotifications(ctx, notifCh); err != nil && ctx.Err() == nil {
			slog.Error("notification subscription failed", "error", err)
		}
	}()

	ticker := time.NewTicker(arrayPollInterval)
	defer ticker.Stop()

	// First poll runs async so a slow/unreachable Unraid host at startup
	// doesn't delay notification delivery or ctx-cancellation handling.
	go t.pollArray(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			t.pollArray(ctx)
		case notif := <-notifCh:
			t.handleNotification(ctx, notif)
		}
	}
}

func (t *Tracker) pollArray(ctx context.Context) {
	status, err := t.gql.QueryArray(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("array query failed", "error", err)
		}
		return
	}
	t.handleArrayStatus(ctx, *status)
}

func (t *Tracker) handleArrayStatus(ctx context.Context, status graphql.ArrayStatus) {
	t.handleParityCheck(ctx, status)
	t.handleArrayState(ctx, status)
}

func (t *Tracker) handleParityCheck(ctx context.Context, status graphql.ArrayStatus) {
	slug := "unraid-parity"
	endedTTL := int(t.cfg.PushWard.CleanupDelay.Seconds())
	staleTTL := int(t.cfg.PushWard.StaleTimeout.Seconds())
	serverName := t.cfg.Unraid.ServerName

	t.mu.Lock()
	wasActive := t.parityActive

	isActive := status.ParityCheck != nil && status.ParityCheck.IsActive()

	if isActive && !wasActive {
		// Parity check started — create activity + seed. parityActive flips to
		// true only after the seed PATCH lands so a failure here means the next
		// poll retries both create and seed.
		t.mu.Unlock()

		if err := t.pw.CreateActivity(ctx, slug, "Parity Check", t.cfg.PushWard.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create parity activity", "error", err)
			return
		}
		if err := t.seedParityUpdate(ctx, slug, *status.ParityCheck, serverName); err != nil {
			slog.Error("failed to seed parity activity", "error", err)
			return
		}

		t.mu.Lock()
		t.parityActive = true
		t.parityLastSent = time.Now()
		t.mu.Unlock()
		return
	}

	if isActive && wasActive {
		// Debounce: only update every 30s
		if time.Since(t.parityLastSent) < 30*time.Second {
			t.mu.Unlock()
			return
		}
		t.mu.Unlock()
		t.tickParityUpdate(ctx, slug, *status.ParityCheck)
		return
	}

	if !isActive && wasActive {
		// Parity check completed
		t.parityActive = false
		t.mu.Unlock()
		t.scheduleEnd(slug, pushward.Content{
			Template:    pushward.TemplateGeneric,
			Progress:    1.0,
			State:       "Parity Valid",
			Icon:        "checkmark.circle.fill",
			Subtitle:    "Unraid · " + serverName,
			AccentColor: pushward.ColorGreen,
		})
		return
	}

	t.mu.Unlock()
}

func deriveParityFrame(pc graphql.ParityCheck) (progress float64, state string) {
	return pc.Progress / 100.0, fmt.Sprintf("Checking · %.0f%%", pc.Progress)
}

func (t *Tracker) seedParityUpdate(ctx context.Context, slug string, pc graphql.ParityCheck, serverName string) error {
	progress, state := deriveParityFrame(pc)
	return t.pw.UpdateActivity(ctx, slug, pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:    pushward.TemplateGeneric,
			Progress:    progress,
			State:       state,
			Icon:        "arrow.triangle.2.circlepath",
			Subtitle:    "Unraid · " + serverName,
			AccentColor: pushward.ColorBlue,
		},
	})
}

func (t *Tracker) tickParityUpdate(ctx context.Context, slug string, pc graphql.ParityCheck) {
	progress, state := deriveParityFrame(pc)
	if err := t.pw.PatchActivity(ctx, slug, pushward.PatchRequest{
		State: pushward.StateOngoing,
		Content: &pushward.ContentPatch{
			Progress: pushward.Float64Ptr(progress),
			State:    pushward.StringPtr(state),
		},
	}); err != nil {
		slog.Error("failed to update parity activity", "error", err)
		return
	}

	t.mu.Lock()
	t.parityLastSent = time.Now()
	t.mu.Unlock()
}

// handleArrayState fires Live Activities on STARTED<->STOPPED transitions.
// Unraid's ArrayState enum has no STARTING/STOPPING — the schema exposes
// only terminal states — so we render the two-phase end directly on the
// transition.
func (t *Tracker) handleArrayState(ctx context.Context, status graphql.ArrayStatus) {
	slug := "unraid-array"
	endedTTL := int(t.cfg.PushWard.CleanupDelay.Seconds())
	staleTTL := int(t.cfg.PushWard.StaleTimeout.Seconds())
	serverName := t.cfg.Unraid.ServerName

	t.mu.Lock()
	prevState := t.arrayState
	if prevState == status.State {
		t.mu.Unlock()
		return
	}
	t.arrayState = status.State
	t.mu.Unlock()

	if prevState == "" {
		return // first observation seeds state without firing an activity
	}

	var content pushward.Content
	switch {
	case prevState == graphql.ArrayStateStopped && status.State == graphql.ArrayStateStarted:
		content = pushward.Content{
			Template:    pushward.TemplateGeneric,
			Progress:    1.0,
			State:       "Array Started",
			Icon:        "checkmark.circle.fill",
			Subtitle:    "Unraid · " + serverName,
			AccentColor: pushward.ColorGreen,
		}
	case prevState == graphql.ArrayStateStarted && status.State == graphql.ArrayStateStopped:
		content = pushward.Content{
			Template:    pushward.TemplateGeneric,
			Progress:    1.0,
			State:       "Array Stopped",
			Icon:        "checkmark.circle.fill",
			Subtitle:    "Unraid · " + serverName,
			AccentColor: pushward.ColorGreen,
		}
	default:
		return // ignore transitions to/from error states (RECON_DISK, etc.)
	}

	if err := t.pw.CreateActivity(ctx, slug, "Array", t.cfg.PushWard.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create array activity", "error", err)
	}
	t.scheduleEnd(slug, content)
}

func (t *Tracker) handleNotification(ctx context.Context, notif graphql.Notification) {
	title := notif.Subject
	if title == "" {
		title = notif.Title
	}
	if strings.TrimSpace(title) == "" {
		return
	}

	level, category, push := mapImportance(notif.Importance)

	serverName := t.cfg.Unraid.ServerName
	subtitle := "Unraid"
	if serverName != "" {
		subtitle = "Unraid · " + serverName
	}

	metadata := map[string]string{}
	if notif.Importance != "" {
		metadata["importance"] = string(notif.Importance)
	}
	if notif.Title != "" && notif.Title != notif.Subject {
		metadata["unraid_title"] = notif.Title
	}
	if notif.ID != "" {
		metadata["unraid_id"] = notif.ID
	}
	if serverName != "" {
		metadata["server"] = serverName
	}

	// PushWard requires a non-empty body. Fall back to the subject when the
	// Unraid notification omits a description (some event types do).
	body := notif.Description
	if strings.TrimSpace(body) == "" {
		body = title
	}

	req := pushward.SendNotificationRequest{
		Title:      text.Truncate(title, 120),
		Subtitle:   text.Truncate(subtitle, 50),
		Body:       text.Truncate(body, 500),
		ThreadID:   "unraid",
		CollapseID: text.SlugHash("unraid-", title+"|"+notif.Title, 6),
		Level:      level,
		Category:   category,
		Source:     "unraid",
		Push:       push,
		Metadata:   metadata,
	}
	if err := t.pw.SendNotification(ctx, req); err != nil {
		slog.Error("failed to send unraid notification", "subject", notif.Subject, "error", err)
	}
}

// mapImportance maps Unraid's notification importance to PushWard's
// interruption level and severity category. ALERT and WARNING are
// active (visible alert); anything else (INFO, empty, unknown) is passive.
// Values come from the Unraid SDL verbatim — uppercase, no aliases.
func mapImportance(importance graphql.Importance) (level, category string, push bool) {
	switch importance {
	case graphql.ImportanceAlert:
		return pushward.LevelActive, pushward.SeverityCritical, true
	case graphql.ImportanceWarning:
		return pushward.LevelActive, pushward.SeverityWarning, true
	default:
		return pushward.LevelPassive, pushward.SeverityInfo, true
	}
}

// scheduleEnd runs a two-phase end for the activity using a timerPair
// so that both timers can be cancelled when a new event arrives.
func (t *Tracker) scheduleEnd(slug string, content pushward.Content) {
	endDelay := t.cfg.PushWard.EndDelay
	displayTime := t.cfg.PushWard.EndDisplayTime

	t.mu.Lock()
	if existing, ok := t.timers[slug]; ok {
		existing.phase1.Stop()
		if existing.phase2 != nil {
			existing.phase2.Stop()
		}
	}
	tp := &timerPair{}
	tp.phase1 = time.AfterFunc(endDelay, func() {
		// Phase 1: ONGOING with final content
		ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel1()
		if err := t.pw.UpdateActivity(ctx1, slug, pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}); err != nil {
			slog.Error("failed to update activity (end phase 1)", "slug", slug, "error", err)
		}

		// Phase 2: schedule ENDED after display time
		t.mu.Lock()
		cur, ok := t.timers[slug]
		if !ok || cur != tp {
			t.mu.Unlock()
			return // cancelled or superseded between phases
		}
		tp.phase2 = time.AfterFunc(displayTime, func() {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel2()
			if err := t.pw.UpdateActivity(ctx2, slug, pushward.UpdateRequest{State: pushward.StateEnded, Content: content}); err != nil {
				slog.Error("failed to update activity (end phase 2)", "slug", slug, "error", err)
			}

			t.mu.Lock()
			delete(t.timers, slug)
			t.mu.Unlock()
		})
		t.mu.Unlock()
	})
	t.timers[slug] = tp
	t.mu.Unlock()
}
