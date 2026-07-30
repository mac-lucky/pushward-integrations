// Package poller adapts the GitHub Actions API to the shared CI poller.
//
// The orchestration - the poll loop, the tracked-run state machine, the
// total-steps clamp, the live-progress anchoring and the two-phase end - lives in
// shared/cipoll. What is left here is GitHub's wire format, its two-pass
// prior-run lookup and its outcome mapping.
package poller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/mac-lucky/pushward-integrations/github/internal/config"
	ghclient "github.com/mac-lucky/pushward-integrations/github/internal/github"
	"github.com/mac-lucky/pushward-integrations/shared/ci"
	"github.com/mac-lucky/pushward-integrations/shared/cipoll"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// slugPrefix namespaces this bridge's per-repo activity slug.
const slugPrefix = "gh"

// seedStatuses orders the prior-run lookups used to seed a stable step total:
// prefer the last fully-successful run (it executed the whole job DAG, so its
// group count is the most accurate), then fall back to any completed run. Note
// GitHub's runs `status` filter accepts both conclusions ("success") and
// statuses ("completed"), so a single "completed" call would not distinguish a
// truncated failed run from a full success - hence both, in this order.
var seedStatuses = []string{ci.ConclusionSuccess, ci.StatusCompleted}

// forge is the GitHub side of cipoll.Forge.
type forge struct {
	gh *ghclient.Client
}

// New wires the GitHub client into a shared poller.
func New(cfg *config.Config, gh *ghclient.Client, pw *pushward.Client) *cipoll.Poller {
	return cipoll.New(&forge{gh: gh}, pw, cipoll.Options{
		Owner:        cfg.GitHub.Owner,
		Repos:        cfg.GitHub.Repos,
		IdleInterval: cfg.Polling.IdleInterval,
		Interval:     cfg.Polling.Interval,
		// GitHub's documented primary limit for a personal access token. Supplied
		// here rather than in cipoll because it is GitHub's number: it is what the
		// loop paces detection against, and what the startup line compares the
		// configured rate to.
		HourlyRequestBudget: ghclient.HourlyRateLimit,
		PushWard:            cfg.PushWard,
		Render:              cfg.Render,
		TitlePrefix:         "GitHub",
		SlugPrefix:          slugPrefix,
		// github.com is a single well-known host that is either reachable or a
		// credential problem, so a failed enumeration at startup is not something
		// to poll through: fail loudly rather than watch a partial repo list.
		DiscoveryRequired: true,
	})
}

func (f *forge) ListRepos(ctx context.Context, owner string) ([]string, error) {
	return f.gh.ListRepos(ctx, owner)
}

// Budget passes GitHub's rate-limit headers, as the client last saw them, up to
// the loop's pacing.
func (f *forge) Budget() (remaining int, resetAt time.Time, ok bool) {
	return f.gh.Budget()
}

func (f *forge) ActiveRuns(ctx context.Context, repo string) ([]cipoll.Run, error) {
	runs, err := f.gh.GetInProgressRuns(ctx, repo)
	if err != nil {
		return nil, err
	}
	out := make([]cipoll.Run, len(runs))
	for i, r := range runs {
		out[i] = toRun(repo, r)
	}
	return out, nil
}

func (f *forge) GetRun(ctx context.Context, repo string, runID int64) (*cipoll.Run, error) {
	run, err := f.gh.GetRun(ctx, repo, runID)
	if err != nil {
		return nil, err
	}
	converted := toRun(repo, *run)
	return &converted, nil
}

func (f *forge) LiveJobs(ctx context.Context, repo string, runID int64) ([]ci.Job, error) {
	jobs, err := f.gh.GetJobs(ctx, repo, runID)
	if err != nil {
		return nil, err
	}
	return toCIJobs(jobs), nil
}

// BaselineJobs looks up a prior finished run of the same workflow and branch.
//
// wantTimings is ignored: GitHub stamps started_at/completed_at on every job it
// returns, so the durations come free with the jobs call and there is no cheaper
// variant to fall back to.
func (f *forge) BaselineJobs(ctx context.Context, repo string, run cipoll.Run, _ bool) (cipoll.Baseline, error) {
	workflowID, err := strconv.ParseInt(run.WorkflowKey, 10, 64)
	if err != nil {
		// Unreachable via the poller, which short-circuits a blank key, but a
		// malformed id must not be turned into a lookup for workflow 0.
		return cipoll.Baseline{}, fmt.Errorf("workflow key %q: %w", run.WorkflowKey, err)
	}
	prev, err := f.lastFinishedRun(ctx, repo, workflowID, run.HeadBranch)
	if err != nil {
		return cipoll.Baseline{}, err
	}
	if prev == nil {
		return cipoll.Baseline{}, nil // no prior run to seed from
	}
	jobs, err := f.gh.GetJobs(ctx, repo, prev.ID)
	if err != nil {
		return cipoll.Baseline{}, fmt.Errorf("jobs for prior run %d: %w", prev.ID, err)
	}
	return cipoll.Baseline{Jobs: toCIJobs(jobs), RunID: prev.ID}, nil
}

// lastFinishedRun returns the most relevant finished run to seed step shape from:
// the last successful run (it ran the full DAG, so it's the most accurate), or -
// when none exists (e.g. a brand-new branch) - the last completed run of any
// conclusion. An early-aborted failure may under-count, but the active-poll
// upward clamp then degrades gracefully to a fresh scan.
//
// A nil run with a nil error means neither lookup found one. The first error
// aborts outright rather than falling through: a failed success lookup says
// nothing about whether a successful run exists, so accepting the completed
// fallback there would seed from a truncated run while a better one was available.
func (f *forge) lastFinishedRun(ctx context.Context, repo string, workflowID int64, branch string) (*ghclient.WorkflowRun, error) {
	for _, status := range seedStatuses {
		run, err := f.gh.GetLatestWorkflowRun(ctx, repo, workflowID, branch, status)
		if err != nil {
			return nil, fmt.Errorf("prior-run lookup (status %s): %w", status, err)
		}
		if run != nil {
			return run, nil
		}
	}
	return nil, nil
}

// Outcome collapses every terminal run to success or failure. GitHub reports both
// a status and a conclusion, and the conclusion is authoritative; the ladder's
// anyFailed only covers a run that reports no conclusion at all.
func (f *forge) Outcome(run cipoll.Run, anyFailed bool) (state, color string) {
	if ci.JobFailed(run.Conclusion) || (run.Conclusion == "" && anyFailed) {
		return cipoll.OutcomeFailed, pushward.ColorRed
	}
	return cipoll.OutcomeSuccess, pushward.ColorGreen
}

// toRun converts a workflow run into the shared poller's shape. GitHub already
// speaks the status/conclusion vocabulary the ladder uses, so only the workflow
// key and the repo link need building.
func toRun(repo string, w ghclient.WorkflowRun) cipoll.Run {
	return cipoll.Run{
		ID:          w.ID,
		Name:        w.Name,
		WorkflowKey: workflowKey(w.WorkflowID),
		Status:      w.Status,
		Conclusion:  w.Conclusion,
		RawStatus:   w.Status,
		HeadBranch:  w.HeadBranch,
		CreatedAt:   w.CreatedAt,
		HTMLURL:     w.HTMLURL,
		RepoURL:     "https://github.com/" + repo,
	}
}

// workflowKey renders a workflow id for the prior-run lookup. Zero stays blank:
// it cannot target a workflow, and the shared loop reads a blank key as "no seed
// possible" and skips the lookup rather than querying workflow 0.
func workflowKey(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}
