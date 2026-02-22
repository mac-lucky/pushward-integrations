package poller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mac-lucky/pushward-docker/github/internal/config"
	ghclient "github.com/mac-lucky/pushward-docker/github/internal/github"
	"github.com/mac-lucky/pushward-docker/github/internal/pushward"
)

type Poller struct {
	cfg *config.Config
	gh  *ghclient.Client
	pw  *pushward.Client

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
	if err := p.refreshRepos(ctx); err != nil {
		return fmt.Errorf("initial repo discovery: %w", err)
	}

	for {
		interval := p.cfg.Polling.IdleInterval
		for _, t := range p.tracked {
			if t.EndedAt == nil {
				interval = p.cfg.Polling.ActiveInterval
				break
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}

		if err := p.refreshRepos(ctx); err != nil {
			slog.Error("repo refresh failed", "error", err)
		}

		slog.Info("poll tick", "repos", len(p.repos), "tracked", len(p.tracked))

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
	p.cleanup(ctx)

	if err := p.pollIdle(ctx); err != nil {
		return err
	}
	return p.pollActive(ctx)
}

func (p *Poller) pollIdle(ctx context.Context) error {
	for _, repo := range p.repos {
		// Skip repos that already have an active entry
		if t, ok := p.tracked[repo]; ok && t.EndedAt == nil {
			continue
		}

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
		if err := p.pw.CreateActivity(ctx, slug, fmt.Sprintf("GitHub: %s", repoShort), p.cfg.PushWard.Priority); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			continue
		}

		p.tracked[repo] = &trackedRun{
			Repo:       repo,
			RunID:      run.ID,
			Name:       run.Name,
			Branch:     run.HeadBranch,
			Slug:       slug,
			HTMLURL:    run.HTMLURL,
			StartedAt:  run.CreatedAt,
			LastUpdate: time.Now(),
		}

		// Send initial ONGOING (triggers push-to-start)
		if err := p.pw.UpdateActivity(ctx, slug, pushward.UpdateRequest{
			State: "ONGOING",
			Content: pushward.Content{
				Template:     "pipeline",
				Progress:     0.0,
				State:        "Starting...",
				Icon:         "arrow.triangle.branch",
				Subtitle:     fmt.Sprintf("%s / %s", repoShort, run.Name),
				AccentColor:  "green",
				CurrentStep:  intPtr(0),
				TotalSteps:   intPtr(1),
				URL:          run.HTMLURL,
				SecondaryURL: fmt.Sprintf("https://github.com/%s", repo),
			},
		}); err != nil {
			slog.Error("failed to send initial update", "slug", slug, "error", err)
		}
	}
	return nil
}

func (p *Poller) pollActive(ctx context.Context) error {
	for repo, t := range p.tracked {
		if t.EndedAt != nil {
			continue
		}

		// Check for stale tracked workflow (30min timeout)
		if time.Since(t.LastUpdate) > 30*time.Minute {
			slog.Warn("tracked workflow stale, ending", "run_id", t.RunID, "slug", t.Slug)
			p.endWorkflow(ctx, t, "Timed out", "orange")
			continue
		}

		jobs, err := p.gh.GetJobs(ctx, t.Repo, t.RunID)
		if err != nil {
			slog.Error("getting jobs", "repo", repo, "error", err)
			continue
		}

		totalJobs := len(jobs)
		if totalJobs == 0 {
			continue
		}

		completedJobs := 0
		var currentJobName string
		currentJobIndex := 0
		allCompleted := true
		anyFailed := false

		for i, job := range jobs {
			switch job.Status {
			case "completed":
				completedJobs++
				if job.Conclusion == "failure" || job.Conclusion == "cancelled" {
					anyFailed = true
				}
			case "in_progress":
				if currentJobName == "" {
					currentJobName = job.Name
					currentJobIndex = i + 1
				}
				allCompleted = false
			default: // queued
				allCompleted = false
			}
		}

		if currentJobName == "" && !allCompleted {
			currentJobName = "Queued"
			currentJobIndex = completedJobs
		}

		t.LastUpdate = time.Now()
		repoShort := repoName(t.Repo)

		if allCompleted {
			conclusion := "Success"
			color := "green"
			if anyFailed {
				conclusion = "Failed"
				color = "red"
			}
			slog.Info("workflow completed", "run_id", t.RunID, "slug", t.Slug, "conclusion", conclusion)
			p.endWorkflow(ctx, t, conclusion, color)
			continue
		}

		progress := float64(completedJobs) / float64(totalJobs)

		if err := p.pw.UpdateActivity(ctx, t.Slug, pushward.UpdateRequest{
			State: "ONGOING",
			Content: pushward.Content{
				Template:     "pipeline",
				Progress:     progress,
				State:        currentJobName,
				Icon:         "arrow.triangle.branch",
				Subtitle:     fmt.Sprintf("%s / %s", repoShort, t.Name),
				AccentColor:  "green",
				CurrentStep:  intPtr(currentJobIndex),
				TotalSteps:   intPtr(totalJobs),
				URL:          t.HTMLURL,
				SecondaryURL: fmt.Sprintf("https://github.com/%s", t.Repo),
			},
		}); err != nil {
			slog.Error("failed to update activity", "slug", t.Slug, "error", err)
		}
	}
	return nil
}

func (p *Poller) endWorkflow(ctx context.Context, t *trackedRun, state, color string) {
	repoShort := repoName(t.Repo)
	if err := p.pw.UpdateActivity(ctx, t.Slug, pushward.UpdateRequest{
		State: "ENDED",
		Content: pushward.Content{
			Template:     "pipeline",
			Progress:     1.0,
			State:        state,
			Icon:         "arrow.triangle.branch",
			Subtitle:     fmt.Sprintf("%s / %s", repoShort, t.Name),
			AccentColor:  color,
			CurrentStep:  intPtr(1),
			TotalSteps:   intPtr(1),
			URL:          t.HTMLURL,
			SecondaryURL: fmt.Sprintf("https://github.com/%s", t.Repo),
		},
	}); err != nil {
		slog.Error("failed to end activity", "slug", t.Slug, "error", err)
	}
	now := time.Now()
	t.EndedAt = &now
}

func (p *Poller) cleanup(ctx context.Context) {
	for repo, t := range p.tracked {
		if t.EndedAt == nil {
			continue
		}
		if time.Since(*t.EndedAt) < p.cfg.PushWard.CleanupDelay {
			continue
		}
		slog.Info("cleaning up activity", "slug", t.Slug, "repo", repo)
		if err := p.pw.DeleteActivity(ctx, t.Slug); err != nil {
			slog.Error("failed to delete activity", "slug", t.Slug, "error", err)
		}
		delete(p.tracked, repo)
	}
}

func intPtr(v int) *int {
	return &v
}

func repoName(fullRepo string) string {
	parts := splitRepo(fullRepo)
	if len(parts) == 2 {
		return parts[1]
	}
	return fullRepo
}

func splitRepo(repo string) []string {
	for i, c := range repo {
		if c == '/' {
			return []string{repo[:i], repo[i+1:]}
		}
	}
	return []string{repo}
}
