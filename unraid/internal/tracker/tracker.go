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

// Tracker monitors Unraid array status and notifications. Parity checks and
// array state transitions are rendered as PushWard Live Activities; every
// Unraid notification is forwarded to the PushWard notification API.
type Tracker struct {
	cfg *config.Config
	gql *graphql.Client
	pw  *pushward.Client

	mu             sync.Mutex
	parityActive   bool
	parityLastSent time.Time
	arrayState     string
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

// Run starts subscriptions and processes events until ctx is cancelled.
func (t *Tracker) Run(ctx context.Context) error {
	arrayCh := make(chan graphql.ArrayStatus, 10)
	notifCh := make(chan graphql.Notification, 10)

	go func() {
		if err := t.gql.SubscribeArray(ctx, arrayCh); err != nil && ctx.Err() == nil {
			slog.Error("array subscription failed", "error", err)
		}
	}()
	go func() {
		if err := t.gql.SubscribeNotifications(ctx, notifCh); err != nil && ctx.Err() == nil {
			slog.Error("notification subscription failed", "error", err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case status := <-arrayCh:
			t.handleArrayStatus(ctx, status)
		case notif := <-notifCh:
			t.handleNotification(ctx, notif)
		}
	}
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

	isActive := status.ParityCheck.IsActive()

	if isActive && !wasActive {
		// Parity check started
		t.parityActive = true
		t.parityLastSent = time.Time{}
		t.mu.Unlock()

		if err := t.pw.CreateActivity(ctx, slug, "Parity Check", t.cfg.PushWard.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create parity activity", "error", err)
			return
		}
		t.sendParityUpdate(ctx, slug, status.ParityCheck, serverName)
		return
	}

	if isActive && wasActive {
		// Debounce: only update every 30s
		if time.Since(t.parityLastSent) < 30*time.Second {
			t.mu.Unlock()
			return
		}
		t.mu.Unlock()
		t.sendParityUpdate(ctx, slug, status.ParityCheck, serverName)
		return
	}

	if !isActive && wasActive {
		// Parity check completed
		t.parityActive = false
		t.mu.Unlock()
		t.scheduleEnd(slug, pushward.Content{
			Template:    "generic",
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

func (t *Tracker) sendParityUpdate(ctx context.Context, slug string, pc graphql.ParityCheck, serverName string) {
	progress := pc.Progress / 100.0
	state := fmt.Sprintf("Checking · %.0f%%", pc.Progress)

	content := pushward.Content{
		Template:    "generic",
		Progress:    progress,
		State:       state,
		Icon:        "arrow.triangle.2.circlepath",
		Subtitle:    "Unraid · " + serverName,
		AccentColor: pushward.ColorBlue,
	}

	req := pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}
	if err := t.pw.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update parity activity", "error", err)
		return
	}

	t.mu.Lock()
	t.parityLastSent = time.Now()
	t.mu.Unlock()
}

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
		return
	}

	switch status.State {
	case "STARTING":
		if err := t.pw.CreateActivity(ctx, slug, "Array", t.cfg.PushWard.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create array activity", "error", err)
		}
		req := pushward.UpdateRequest{
			State: pushward.StateOngoing,
			Content: pushward.Content{
				Template:    "generic",
				Progress:    0.5,
				State:       "Starting...",
				Icon:        "arrow.triangle.2.circlepath",
				Subtitle:    "Unraid · " + serverName,
				AccentColor: pushward.ColorBlue,
			},
		}
		if err := t.pw.UpdateActivity(ctx, slug, req); err != nil {
			slog.Error("failed to update array activity", "error", err)
		}

	case "STARTED":
		if prevState == "STARTING" {
			t.scheduleEnd(slug, pushward.Content{
				Template:    "generic",
				Progress:    1.0,
				State:       "Array Started",
				Icon:        "checkmark.circle.fill",
				Subtitle:    "Unraid · " + serverName,
				AccentColor: pushward.ColorGreen,
			})
		}

	case "STOPPING":
		if err := t.pw.CreateActivity(ctx, slug, "Array", t.cfg.PushWard.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create array activity", "error", err)
		}
		req := pushward.UpdateRequest{
			State: pushward.StateOngoing,
			Content: pushward.Content{
				Template:    "generic",
				Progress:    0.5,
				State:       "Stopping...",
				Icon:        "arrow.triangle.2.circlepath",
				Subtitle:    "Unraid · " + serverName,
				AccentColor: pushward.ColorOrange,
			},
		}
		if err := t.pw.UpdateActivity(ctx, slug, req); err != nil {
			slog.Error("failed to update array activity", "error", err)
		}

	case "STOPPED":
		if prevState == "STOPPING" {
			t.scheduleEnd(slug, pushward.Content{
				Template:    "generic",
				Progress:    1.0,
				State:       "Array Stopped",
				Icon:        "checkmark.circle.fill",
				Subtitle:    "Unraid · " + serverName,
				AccentColor: pushward.ColorGreen,
			})
		}
	}
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
		metadata["importance"] = notif.Importance
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
// interruption level and severity category. `alert` and `warning` are
// active (visible alert); anything else (info, notice, empty) is passive.
func mapImportance(importance string) (level, category string, push bool) {
	switch importance {
	case "alert":
		return pushward.LevelActive, pushward.SeverityCritical, true
	case "warning":
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

