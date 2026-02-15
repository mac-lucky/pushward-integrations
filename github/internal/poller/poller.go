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

	tracked *trackedRun
}

func New(cfg *config.Config, gh *ghclient.Client, pw *pushward.Client) *Poller {
	return &Poller{cfg: cfg, gh: gh, pw: pw}
}

func (p *Poller) Run(ctx context.Context) error {
	for {
		interval := p.cfg.Polling.IdleInterval
		if p.tracked != nil {
			interval = p.cfg.Polling.ActiveInterval
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}

		if err := p.poll(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("poll error", "error", err)
		}
	}
}

func (p *Poller) poll(ctx context.Context) error {
	if p.tracked != nil {
		return p.pollActive(ctx)
	}
	return p.pollIdle(ctx)
}

func (p *Poller) pollIdle(ctx context.Context) error {
	for _, repo := range p.cfg.GitHub.Repos {
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

		slog.Info("workflow found", "repo", repo, "run_id", run.ID, "name", run.Name, "branch", run.HeadBranch)
		p.tracked = &trackedRun{
			Repo:       repo,
			RunID:      run.ID,
			Name:       run.Name,
			Branch:     run.HeadBranch,
			StartedAt:  run.CreatedAt,
			LastUpdate: time.Now(),
		}

		// Send initial ONGOING (triggers push-to-start)
		repoShort := repoName(repo)
		return p.pw.UpdateActivity(ctx, p.cfg.PushWard.ActivitySlug, pushward.UpdateRequest{
			State: "ONGOING",
			Content: pushward.Content{
				Template:    "github",
				Progress:    0.0,
				State:       "Starting...",
				Icon:        "arrow.triangle.branch",
				Subtitle:    fmt.Sprintf("%s / %s", repoShort, run.Name),
				AccentColor: "green",
				CurrentStep: intPtr(0),
				TotalSteps:  intPtr(1),
			},
		})
	}
	return nil
}

func (p *Poller) pollActive(ctx context.Context) error {
	// Check for stale tracked workflow (30min timeout)
	if time.Since(p.tracked.LastUpdate) > 30*time.Minute {
		slog.Warn("tracked workflow stale, ending", "run_id", p.tracked.RunID)
		return p.endWorkflow(ctx, "Timed out", "orange")
	}

	jobs, err := p.gh.GetJobs(ctx, p.tracked.Repo, p.tracked.RunID)
	if err != nil {
		return fmt.Errorf("getting jobs: %w", err)
	}

	totalJobs := len(jobs)
	if totalJobs == 0 {
		return nil
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

	p.tracked.LastUpdate = time.Now()
	repoShort := repoName(p.tracked.Repo)

	if allCompleted {
		conclusion := "Success"
		color := "green"
		if anyFailed {
			conclusion = "Failed"
			color = "red"
		}
		slog.Info("workflow completed", "run_id", p.tracked.RunID, "conclusion", conclusion)
		return p.endWorkflow(ctx, conclusion, color)
	}

	progress := float64(completedJobs) / float64(totalJobs)

	return p.pw.UpdateActivity(ctx, p.cfg.PushWard.ActivitySlug, pushward.UpdateRequest{
		State: "ONGOING",
		Content: pushward.Content{
			Template:    "github",
			Progress:    progress,
			State:       currentJobName,
			Icon:        "arrow.triangle.branch",
			Subtitle:    fmt.Sprintf("%s / %s", repoShort, p.tracked.Name),
			AccentColor: "green",
			CurrentStep: intPtr(currentJobIndex),
			TotalSteps:  intPtr(totalJobs),
		},
	})
}

func (p *Poller) endWorkflow(ctx context.Context, state, color string) error {
	total := 1
	if p.tracked != nil {
		// Best effort: use last known total
	}
	repoShort := repoName(p.tracked.Repo)
	err := p.pw.UpdateActivity(ctx, p.cfg.PushWard.ActivitySlug, pushward.UpdateRequest{
		State: "ENDED",
		Content: pushward.Content{
			Template:    "github",
			Progress:    1.0,
			State:       state,
			Icon:        "arrow.triangle.branch",
			Subtitle:    fmt.Sprintf("%s / %s", repoShort, p.tracked.Name),
			AccentColor: color,
			CurrentStep: intPtr(total),
			TotalSteps:  intPtr(total),
		},
	})
	p.tracked = nil
	return err
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
