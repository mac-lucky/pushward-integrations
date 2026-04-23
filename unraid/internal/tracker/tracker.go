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

// Tracker monitors Unraid array status and notifications, creating
// PushWard Live Activities for parity checks, array state changes,
// and critical notifications (disk errors, UPS events).
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
	t.arrayState = status.State
	t.mu.Unlock()

	if prevState == status.State || prevState == "" {
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
	serverName := t.cfg.Unraid.ServerName
	endedTTL := int(t.cfg.PushWard.CleanupDelay.Seconds())
	staleTTL := int(t.cfg.PushWard.StaleTimeout.Seconds())

	switch {
	case strings.Contains(notif.Subject, "SMART") ||
		strings.Contains(notif.Subject, "disk") ||
		strings.Contains(notif.Subject, "Disk"):
		slug := fmt.Sprintf("unraid-disk-%s", sanitize(notif.Subject))
		if err := t.pw.CreateActivity(ctx, slug, text.TruncateHard(notif.Subject, 100), t.cfg.PushWard.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create disk activity", "slug", slug, "error", err)
		}
		content := pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       text.TruncateHard(notif.Description, 100),
			Icon:        "exclamationmark.octagon.fill",
			Subtitle:    text.TruncateHard("Unraid · "+serverName, 50),
			AccentColor: pushward.ColorRed,
			Severity:    "error",
		}
		req := pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}
		if err := t.pw.UpdateActivity(ctx, slug, req); err != nil {
			slog.Error("failed to update disk activity", "slug", slug, "error", err)
		}
		t.scheduleEnd(slug, content)

	case strings.Contains(notif.Subject, "UPS") ||
		strings.Contains(notif.Subject, "ups") ||
		strings.Contains(notif.Subject, "battery") ||
		strings.Contains(notif.Subject, "Battery"):
		slug := "unraid-ups"
		if err := t.pw.CreateActivity(ctx, slug, "UPS Event", t.cfg.PushWard.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create UPS activity", "error", err)
		}

		accentColor := pushward.ColorOrange
		icon := "bolt.slash.fill"
		severity := "warning"
		if notif.Importance == "alert" {
			accentColor = pushward.ColorRed
			severity = "error"
		}

		content := pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       text.TruncateHard(notif.Subject, 100),
			Icon:        icon,
			Subtitle:    text.TruncateHard("Unraid · "+serverName, 50),
			AccentColor: accentColor,
			Severity:    severity,
		}
		req := pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}
		if err := t.pw.UpdateActivity(ctx, slug, req); err != nil {
			slog.Error("failed to update UPS activity", "error", err)
		}
		t.scheduleEnd(slug, content)

	default:
		slog.Debug("unraid notification ignored", "subject", notif.Subject, "title", notif.Title)
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

func sanitize(s string) string {
	var result []byte
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result = append(result, c)
		} else if c >= 'A' && c <= 'Z' {
			result = append(result, c+32) // toLower
		} else {
			if len(result) > 0 && result[len(result)-1] != '-' {
				result = append(result, '-')
			}
		}
	}
	if len(result) > 20 {
		result = result[:20]
	}
	// Strip trailing dashes
	for len(result) > 0 && result[len(result)-1] == '-' {
		result = result[:len(result)-1]
	}
	return string(result)
}
