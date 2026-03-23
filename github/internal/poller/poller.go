package poller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/github/internal/config"
	ghclient "github.com/mac-lucky/pushward-integrations/github/internal/github"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

type Poller struct {
	cfg *config.Config
	gh  *ghclient.Client
	pw  *pushward.Client

	mu          sync.Mutex
	tracked     map[string]*trackedRun
	repos       []string
	lastRefresh time.Time
}

const repoRefreshInterval = 5 * time.Minute

func New(cfg *config.Config, gh *ghclient.Client, pw *pushward.Client) *Poller {
	return &Poller{
		cfg:     cfg,
		gh:      gh,
		pw:      pw,
		tracked: make(map[string]*trackedRun),
		repos:   cfg.GitHub.Repos,
	}
}

func (p *Poller) Run(ctx context.Context) error {
	defer func() {
		p.mu.Lock()
		for _, t := range p.tracked {
			if t.endTimers != nil {
				t.endTimers.phase1.Stop()
				if t.endTimers.phase2 != nil {
					t.endTimers.phase2.Stop()
				}
			}
		}
		p.mu.Unlock()
	}()

	if err := p.refreshRepos(ctx); err != nil {
		return fmt.Errorf("initial repo discovery: %w", err)
	}

	// First check immediately on startup.
	if err := p.poll(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		slog.Error("initial poll error", "error", err)
	}

	ticker := time.NewTicker(p.cfg.Polling.IdleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		if err := p.refreshRepos(ctx); err != nil {
			slog.Error("repo refresh failed", "error", err)
		}

		if err := p.poll(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("poll error", "error", err)
		}
	}
}

func (p *Poller) refreshRepos(ctx context.Context) error {
	if p.cfg.GitHub.Owner == "" {
		return nil
	}
	if !p.lastRefresh.IsZero() && time.Since(p.lastRefresh) < repoRefreshInterval {
		return nil
	}

	discovered, err := p.gh.ListRepos(ctx, p.cfg.GitHub.Owner)
	if err != nil {
		return err
	}

	// Merge: discovered repos + any explicitly configured repos
	seen := make(map[string]bool, len(discovered)+len(p.cfg.GitHub.Repos))
	var merged []string
	for _, r := range discovered {
		if !seen[r] {
			seen[r] = true
			merged = append(merged, r)
		}
	}
	for _, r := range p.cfg.GitHub.Repos {
		if !seen[r] {
			seen[r] = true
			merged = append(merged, r)
		}
	}

	if len(merged) != len(p.repos) {
		slog.Info("repo list updated", "count", len(merged))
	}
	p.repos = merged
	p.lastRefresh = time.Now()
	return nil
}

func (p *Poller) poll(ctx context.Context) error {
	if err := p.pollIdle(ctx); err != nil {
		return err
	}
	return p.pollActive(ctx)
}

func (p *Poller) pollIdle(ctx context.Context) error {
	for _, repo := range p.repos {
		// Skip repos that already have an active entry (no pending end)
		p.mu.Lock()
		existing, ok := p.tracked[repo]
		if ok && existing.endTimers == nil {
			p.mu.Unlock()
			continue
		}
		// If existing entry has a pending end timer, cancel it and allow new run
		if ok && existing.endTimers != nil {
			existing.endTimers.phase1.Stop()
			if existing.endTimers.phase2 != nil {
				existing.endTimers.phase2.Stop()
			}
			delete(p.tracked, repo)
			slog.Info("cancelled pending end for new workflow", "repo", repo, "slug", existing.Slug)
		}
		p.mu.Unlock()

		runs, err := p.gh.GetInProgressRuns(ctx, repo)
		if err != nil {
			slog.Error("failed to get runs", "repo", repo, "error", err)
			continue
		}
		if len(runs) == 0 {
			continue
		}

		// Pick the most recently created run
		run := runs[0]
		for _, r := range runs[1:] {
			if r.CreatedAt.After(run.CreatedAt) {
				run = r
			}
		}

		repoShort := repoName(repo)
		slug := fmt.Sprintf("gh-%s", repoShort)

		slog.Info("workflow found", "repo", repo, "run_id", run.ID, "name", run.Name, "branch", run.HeadBranch, "slug", slug)

		// Create the activity in PushWard
		endedTTL := int(p.cfg.PushWard.CleanupDelay.Seconds())
		staleTTL := int(p.cfg.PushWard.StaleTimeout.Seconds())
		if err := p.pw.CreateActivity(ctx, slug, fmt.Sprintf("GitHub: %s", repoShort), p.cfg.PushWard.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			continue
		}

		p.mu.Lock()
		p.tracked[repo] = &trackedRun{
			Repo:       repo,
			RunID:      run.ID,
			Name:       run.Name,
			Slug:       slug,
			HTMLURL:    run.HTMLURL,
			LastUpdate: time.Now(),
		}
		p.mu.Unlock()

		// Fetch jobs for accurate initial step count.
		initialTotalSteps := 1
		var initialStepRows []int
		var initialStepLabels []string
		if jobs, err := p.gh.GetJobs(ctx, repo, run.ID); err != nil {
			slog.Warn("failed to fetch jobs for initial step count, using default",
				"repo", repo, "run_id", run.ID, "error", err)
		} else if len(jobs) > 0 {
			info := computeSteps(jobs)
			initialTotalSteps = info.TotalSteps
			initialStepRows = info.StepRows
			initialStepLabels = info.StepLabels
			slog.Info("initial job scan",
				"repo", repo, "jobs", len(jobs),
				"steps", info.TotalSteps, "step_rows", info.StepRows)
		}

		// Store max totals on the tracked run.
		p.mu.Lock()
		if t, ok := p.tracked[repo]; ok {
			t.maxTotalSteps = initialTotalSteps
			t.maxStepRows = append([]int(nil), initialStepRows...)
			t.maxStepLabels = append([]string(nil), initialStepLabels...)
		}
		p.mu.Unlock()

		// Send initial ONGOING (triggers push-to-start)
		if err := p.pw.UpdateActivity(ctx, slug, pushward.UpdateRequest{
			State: pushward.StateOngoing,
			Content: pushward.Content{
				Template:     "pipeline",
				Progress:     0.0,
				State:        "Starting...",
				Icon:         "arrow.triangle.branch",
				Subtitle:     fmt.Sprintf("%s / %s", repoShort, run.Name),
				AccentColor:  "green",
				CurrentStep:  pushward.IntPtr(0),
				TotalSteps:   pushward.IntPtr(initialTotalSteps),
				StepRows:     initialStepRows,
				StepLabels:   initialStepLabels,
				URL:          run.HTMLURL,
				SecondaryURL: fmt.Sprintf("https://github.com/%s", repo),
			},
		}); err != nil {
			slog.Error("failed to send initial update", "slug", slug, "error", err)
		}
	}
	return nil
}

// stepInfo holds computed pipeline step information from a set of jobs.
type stepInfo struct {
	TotalSteps      int
	CurrentStep     int
	CurrentStepName string
	StepRows        []int
	StepLabels      []string
	AllCompleted    bool
	AnyFailed       bool
	Progress        float64
}

// computeSteps groups jobs by base name (supporting matrix strategies) and
// computes pipeline step progress information.
func computeSteps(jobs []ghclient.Job) stepInfo {
	type step struct {
		name      string
		count     int
		completed int
		active    bool
		failed    bool
	}
	var steps []step
	stepIdx := make(map[string]int)
	completedJobs := 0
	allCompleted := true
	anyFailed := false

	for _, job := range jobs {
		base := baseJobName(job.Name)
		si, ok := stepIdx[base]
		if !ok {
			si = len(steps)
			stepIdx[base] = si
			steps = append(steps, step{name: base})
		}
		steps[si].count++

		switch job.Status {
		case "completed":
			completedJobs++
			steps[si].completed++
			if job.Conclusion == "failure" || job.Conclusion == "cancelled" {
				steps[si].failed = true
				anyFailed = true
			}
		case "in_progress":
			steps[si].active = true
			allCompleted = false
		default: // queued
			allCompleted = false
		}
	}

	totalSteps := len(steps)
	stepRows := make([]int, totalSteps)
	stepLabels := make([]string, totalSteps)
	currentStep := 0
	var currentStepName string

	for i, s := range steps {
		stepRows[i] = s.count
		stepLabels[i] = s.name
		if s.active && currentStepName == "" {
			currentStepName = s.name
			currentStep = i + 1
		}
	}

	if currentStepName == "" && !allCompleted {
		currentStepName = "Queued"
		for i, s := range steps {
			if s.completed < s.count {
				currentStep = i
				break
			}
		}
	}

	progress := 0.0
	if len(jobs) > 0 {
		progress = float64(completedJobs) / float64(len(jobs))
	}

	return stepInfo{
		TotalSteps:      totalSteps,
		CurrentStep:     currentStep,
		CurrentStepName: currentStepName,
		StepRows:        stepRows,
		StepLabels:      stepLabels,
		AllCompleted:    allCompleted,
		AnyFailed:       anyFailed,
		Progress:        progress,
	}
}

func (p *Poller) pollActive(ctx context.Context) error {
	// Snapshot tracked keys under lock to avoid holding mutex across network calls
	p.mu.Lock()
	repos := make([]string, 0, len(p.tracked))
	for repo := range p.tracked {
		repos = append(repos, repo)
	}
	p.mu.Unlock()

	for _, repo := range repos {
		p.mu.Lock()
		t, ok := p.tracked[repo]
		if !ok || t.endTimers != nil {
			p.mu.Unlock()
			continue
		}
		// Copy values needed for network calls
		tRepo := t.Repo
		tRunID := t.RunID
		tSlug := t.Slug
		tName := t.Name
		tHTMLURL := t.HTMLURL
		p.mu.Unlock()

		jobs, err := p.gh.GetJobs(ctx, tRepo, tRunID)
		if err != nil {
			slog.Error("getting jobs", "repo", repo, "error", err)
			continue
		}

		if len(jobs) == 0 {
			continue
		}

		info := computeSteps(jobs)

		p.mu.Lock()
		if tt, ok := p.tracked[repo]; ok {
			tt.LastUpdate = time.Now()

			// Clamp TotalSteps to never decrease: GitHub lazily creates
			// jobs behind needs/if conditions, so new steps appear over
			// time. We keep the highest total to avoid confusing jumps.
			if info.TotalSteps > tt.maxTotalSteps {
				slog.Info("new steps discovered",
					"repo", repo, "jobs", len(jobs),
					"prev_steps", tt.maxTotalSteps, "new_steps", info.TotalSteps,
					"step_rows", info.StepRows)
				tt.maxTotalSteps = info.TotalSteps
				tt.maxStepRows = append([]int(nil), info.StepRows...)
				tt.maxStepLabels = append([]string(nil), info.StepLabels...)
			} else if info.TotalSteps < tt.maxTotalSteps {
				// Fewer steps than our max (shouldn't happen) — use cached.
				info.TotalSteps = tt.maxTotalSteps
				info.StepRows = tt.maxStepRows
				info.StepLabels = tt.maxStepLabels
			}
		}
		p.mu.Unlock()

		repoShort := repoName(tRepo)

		if info.AllCompleted {
			conclusion := "Success"
			color := "green"
			if info.AnyFailed {
				conclusion = "Failed"
				color = "red"
			}
			slog.Info("workflow completed", "run_id", tRunID, "slug", tSlug, "conclusion", conclusion)
			p.scheduleEnd(repo, pushward.Content{
				Template:     "pipeline",
				Progress:     1.0,
				State:        conclusion,
				Icon:         "arrow.triangle.branch",
				Subtitle:     fmt.Sprintf("%s / %s", repoShort, tName),
				AccentColor:  color,
				CurrentStep:  pushward.IntPtr(info.TotalSteps),
				TotalSteps:   pushward.IntPtr(info.TotalSteps),
				StepRows:     info.StepRows,
				StepLabels:   info.StepLabels,
				URL:          tHTMLURL,
				SecondaryURL: fmt.Sprintf("https://github.com/%s", tRepo),
			})
			continue
		}

		if err := p.pw.UpdateActivity(ctx, tSlug, pushward.UpdateRequest{
			State: pushward.StateOngoing,
			Content: pushward.Content{
				Template:     "pipeline",
				Progress:     info.Progress,
				State:        info.CurrentStepName,
				Icon:         "arrow.triangle.branch",
				Subtitle:     fmt.Sprintf("%s / %s", repoShort, tName),
				AccentColor:  "green",
				CurrentStep:  pushward.IntPtr(info.CurrentStep),
				TotalSteps:   pushward.IntPtr(info.TotalSteps),
				StepRows:     info.StepRows,
				StepLabels:   info.StepLabels,
				URL:          tHTMLURL,
				SecondaryURL: fmt.Sprintf("https://github.com/%s", tRepo),
			},
		}); err != nil {
			slog.Error("failed to update activity", "slug", tSlug, "error", err)
		}
	}
	return nil
}

// scheduleEnd schedules a two-phase end for an activity:
//   - Phase 1 (after EndDelay): ONGOING update with final content (visible in Dynamic Island)
//   - Phase 2 (EndDisplayTime later): ENDED with same content (dismisses Live Activity)
//
// This gives iOS time to register the push-update token after push-to-start,
// and ensures the Dynamic Island shows the final state before dismissal.
func (p *Poller) scheduleEnd(repo string, content pushward.Content) {
	p.mu.Lock()
	t, ok := p.tracked[repo]
	if !ok {
		p.mu.Unlock()
		return
	}
	slug := t.Slug
	runID := t.RunID
	endDelay := p.cfg.PushWard.EndDelay
	displayTime := p.cfg.PushWard.EndDisplayTime

	tp := &timerPair{}
	tp.phase1 = time.AfterFunc(endDelay, func() {
		// Phase 1: ONGOING with final content
		ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
		ongoingReq := pushward.UpdateRequest{
			State:   pushward.StateOngoing,
			Content: content,
		}
		if err := p.pw.UpdateActivity(ctx1, slug, ongoingReq); err != nil {
			slog.Error("failed to update activity (end phase 1)", "slug", slug, "error", err)
		} else {
			slog.Info("updated activity", "slug", slug, "state", content.State)
		}
		cancel1()

		// Phase 2: schedule ENDED after display time
		p.mu.Lock()
		if current, ok := p.tracked[repo]; !ok || current.RunID != runID {
			p.mu.Unlock()
			return // cancelled between phases
		}
		tp.phase2 = time.AfterFunc(displayTime, func() {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel2()
			endedReq := pushward.UpdateRequest{
				State:   pushward.StateEnded,
				Content: content,
			}
			if err := p.pw.UpdateActivity(ctx2, slug, endedReq); err != nil {
				slog.Error("failed to end activity (end phase 2)", "slug", slug, "error", err)
			} else {
				slog.Info("ended activity", "slug", slug, "state", content.State)
			}

			// Server handles cleanup via ended_ttl — just remove from local map
			p.mu.Lock()
			if current, ok := p.tracked[repo]; ok && current.RunID == runID {
				delete(p.tracked, repo)
			}
			p.mu.Unlock()
		})
		p.mu.Unlock()
	})
	t.endTimers = tp
	p.mu.Unlock()
}

func repoName(fullRepo string) string {
	if _, name, ok := strings.Cut(fullRepo, "/"); ok {
		return name
	}
	return fullRepo
}

// baseJobName strips the reusable-workflow caller prefix and matrix
// parameters from a job name.
// "ci-cd / Build (ubuntu, node-16)" → "Build"
// "ci-cd / Setup Build Environment"  → "Setup Build Environment"
// "Test" → "Test"
func baseJobName(name string) string {
	// Strip reusable-workflow caller prefix ("ci-cd / X" → "X").
	if i := strings.Index(name, " / "); i != -1 {
		name = name[i+3:]
	}
	// Strip matrix parameters ("Build (ubuntu, node-16)" → "Build").
	if i := strings.LastIndex(name, " ("); i != -1 && strings.HasSuffix(name, ")") {
		return name[:i]
	}
	return name
}
