package tracker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/mqtt/internal/config"
	"github.com/mac-lucky/pushward-integrations/mqtt/internal/extract"
	"github.com/mac-lucky/pushward-integrations/mqtt/internal/mapper"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

type Config struct {
	EndDelay       time.Duration
	EndDisplayTime time.Duration
	CleanupDelay   time.Duration
	StaleTimeout   time.Duration
	UpdateInterval time.Duration
}

type trackedActivity struct {
	slug       string
	lastSeen   time.Time
	lastSent   time.Time
	endTimer   *time.Timer
	inactTimer *time.Timer // presence mode only
}

type Tracker struct {
	rule       *config.RuleConfig
	pw         *pushward.Client
	priority   int
	cfg        *Config
	mu         sync.Mutex
	activities map[string]*trackedActivity
}

func New(rule *config.RuleConfig, pw *pushward.Client, priority int, cfg *Config) *Tracker {
	return &Tracker{
		rule:       rule,
		pw:         pw,
		priority:   priority,
		cfg:        cfg,
		activities: make(map[string]*trackedActivity),
	}
}

// HandleMessage processes an incoming MQTT message for this rule.
func (t *Tracker) HandleMessage(data map[string]any) {
	slug := t.rule.Slug

	switch t.rule.Lifecycle {
	case "field":
		t.handleField(slug, data)
	case "presence":
		t.handlePresence(slug, data)
	}
}

// Stop cancels all pending timers.
func (t *Tracker) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, act := range t.activities {
		if act.endTimer != nil {
			act.endTimer.Stop()
		}
		if act.inactTimer != nil {
			act.inactTimer.Stop()
		}
	}
}

func (t *Tracker) handleField(slug string, data map[string]any) {
	stateVal, ok := extract.GetString(data, t.rule.StateField)
	if !ok {
		slog.Debug("state field not found in message", "field", t.rule.StateField, "rule", t.rule.Name)
		return
	}

	mapped, ok := t.rule.StateMap[stateVal]
	if !ok {
		slog.Debug("state value not in state_map", "value", stateVal, "rule", t.rule.Name)
		return
	}

	switch mapped {
	case "IGNORE":
		return
	case pushward.StateOngoing:
		t.createOrUpdate(slug, data)
	case pushward.StateEnded:
		t.scheduleEnd(slug, data)
	}
}

func (t *Tracker) handlePresence(slug string, data map[string]any) {
	t.mu.Lock()
	act, exists := t.activities[slug]

	if exists && act.inactTimer != nil {
		act.inactTimer.Stop()
	}

	if exists && act.endTimer != nil {
		// End was scheduled but new message arrived — cancel it
		act.endTimer.Stop()
		act.endTimer = nil
	}

	if !exists {
		t.mu.Unlock()
		t.createActivity(slug, data)
		t.mu.Lock()
		act = t.activities[slug]
		if act == nil {
			t.mu.Unlock()
			return
		}
	}

	act.lastSeen = time.Now()

	// Reset inactivity timer
	act.inactTimer = time.AfterFunc(t.rule.InactivityTimeout, func() {
		t.scheduleEnd(slug, data)
	})

	t.mu.Unlock()

	// Debounced update
	t.sendUpdate(slug, data, pushward.StateOngoing)
}

func (t *Tracker) createOrUpdate(slug string, data map[string]any) {
	t.mu.Lock()
	act, exists := t.activities[slug]
	if exists && act.endTimer != nil {
		act.endTimer.Stop()
		act.endTimer = nil
	}
	t.mu.Unlock()

	if !exists {
		t.createActivity(slug, data)
	}

	t.sendUpdate(slug, data, pushward.StateOngoing)
}

func (t *Tracker) createActivity(slug string, data map[string]any) {
	endedTTL := int(t.cfg.CleanupDelay.Seconds())
	staleTTL := int(t.cfg.StaleTimeout.Seconds())

	priority := t.priority
	if t.rule.Priority != nil {
		priority = *t.rule.Priority
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := t.pw.CreateActivity(ctx, slug, t.rule.Name, priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "slug", slug, "error", err)
		return
	}
	slog.Info("created activity", "slug", slug, "rule", t.rule.Name)

	t.mu.Lock()
	t.activities[slug] = &trackedActivity{
		slug:     slug,
		lastSeen: time.Now(),
	}
	t.mu.Unlock()
}

func (t *Tracker) sendUpdate(slug string, data map[string]any, state string) {
	t.mu.Lock()
	act, exists := t.activities[slug]
	if !exists {
		t.mu.Unlock()
		return
	}

	// Debounce: skip if within update interval (exception: ENDED always goes through)
	if state != pushward.StateEnded && time.Since(act.lastSent) < t.cfg.UpdateInterval {
		t.mu.Unlock()
		return
	}
	act.lastSent = time.Now()
	act.lastSeen = time.Now()
	t.mu.Unlock()

	content := mapper.Map(data, t.rule.Content, t.rule.Template)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := pushward.UpdateRequest{
		State:   state,
		Content: content,
	}
	if err := t.pw.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
	}
}

// scheduleEnd implements the two-phase end pattern:
// Phase 1 (after endDelay): ONGOING with final content
// Phase 2 (after endDisplayTime): ENDED with same content
func (t *Tracker) scheduleEnd(slug string, data map[string]any) {
	t.mu.Lock()
	act, exists := t.activities[slug]
	if !exists {
		t.mu.Unlock()
		return
	}
	if act.endTimer != nil {
		act.endTimer.Stop()
	}
	endDelay := t.cfg.EndDelay
	displayTime := t.cfg.EndDisplayTime
	t.mu.Unlock()

	act.endTimer = time.AfterFunc(endDelay, func() {
		content := mapper.Map(data, t.rule.Content, t.rule.Template)

		// Phase 1: ONGOING with final content
		ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
		req1 := pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}
		if err := t.pw.UpdateActivity(ctx1, slug, req1); err != nil {
			slog.Error("failed to update activity (end phase 1)", "slug", slug, "error", err)
		}
		cancel1()
		slog.Info("two-phase end: sent ONGOING with final content", "slug", slug, "display_time", displayTime)

		// Phase 2: ENDED
		time.AfterFunc(displayTime, func() {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel2()
			req2 := pushward.UpdateRequest{State: pushward.StateEnded, Content: content}
			if err := t.pw.UpdateActivity(ctx2, slug, req2); err != nil {
				slog.Error("failed to end activity (end phase 2)", "slug", slug, "error", err)
			}
			slog.Info("ended activity", "slug", slug)

			t.mu.Lock()
			delete(t.activities, slug)
			t.mu.Unlock()
		})
	})
}
