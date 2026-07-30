// Package poller adapts the Forgejo Actions API to the shared CI poller.
//
// The orchestration - the poll loop, the tracked-run state machine, the
// total-steps clamp, the live-progress anchoring and the two-phase end - lives in
// shared/cipoll. What is left here is Forgejo's URL resolution, its timing-join
// decision and its outcome mapping. The wire translation happens one layer down,
// in internal/forgejo, which normalizes Forgejo's single status field into the
// status/conclusion pair the ladder reads.
package poller

import (
	"context"
	"fmt"

	"github.com/mac-lucky/pushward-integrations/forgejo/internal/config"
	fjclient "github.com/mac-lucky/pushward-integrations/forgejo/internal/forgejo"
	"github.com/mac-lucky/pushward-integrations/shared/ci"
	"github.com/mac-lucky/pushward-integrations/shared/cipoll"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// slugPrefix must differ from the relay's Forgejo route, which owns
// "forgejo-<hash8>" for these same repos. Sharing it would make the poller and
// the webhook handler fight over one activity.
const slugPrefix = "fj"

// forge is the Forgejo side of cipoll.Forge.
type forge struct {
	fj *fjclient.Client
}

// New wires the Forgejo client into a shared poller.
func New(cfg *config.Config, fj *fjclient.Client, pw *pushward.Client) *cipoll.Poller {
	return cipoll.New(&forge{fj: fj}, pw, cipoll.Options{
		Owner:             cfg.Forgejo.Owner,
		Repos:             cfg.Forgejo.Repos,
		IdleInterval:      cfg.Polling.IdleInterval,
		PushWard:          cfg.PushWard,
		Render:            cfg.Render,
		TitlePrefix:       "Forgejo",
		SlugPrefix:        slugPrefix,
		DiscoveryRequired: discoveryRequired(cfg),
	})
}

// discoveryRequired reports whether a failed initial owner enumeration should
// take the bridge down. Only when discovery is the only source of repos: with an
// explicit list configured there is still work to do, and a self-hosted instance
// that is briefly unreachable at boot - or a token without the read:organization
// scope - should not crashloop the bridge.
func discoveryRequired(cfg *config.Config) bool {
	return len(cfg.Forgejo.Repos) == 0
}

func (f *forge) ListRepos(ctx context.Context, owner string) ([]string, error) {
	return f.fj.ListRepos(ctx, owner)
}

func (f *forge) ActiveRuns(ctx context.Context, repo string) ([]cipoll.Run, error) {
	runs, err := f.fj.GetActiveRuns(ctx, repo)
	if err != nil {
		return nil, err
	}
	out := make([]cipoll.Run, len(runs))
	for i, r := range runs {
		out[i] = f.toRun(repo, r)
	}
	return out, nil
}

func (f *forge) GetRun(ctx context.Context, repo string, runID int64) (*cipoll.Run, error) {
	run, err := f.fj.GetRun(ctx, repo, runID)
	if err != nil {
		return nil, err
	}
	converted := f.toRun(repo, *run)
	return &converted, nil
}

func (f *forge) LiveJobs(ctx context.Context, repo string, runID int64) ([]ci.Job, error) {
	jobs, err := f.fj.GetLiveJobs(ctx, repo, runID)
	if err != nil {
		return nil, err
	}
	return toCIJobs(jobs), nil
}

// BaselineJobs looks up a prior finished run of the same workflow and branch.
//
// wantTimings decides how the jobs are fetched: Forgejo's job objects carry no
// timestamps, so the durations cost an extra paginated tasks call that is pure
// waste when neither the pill sizing nor the ETA is switched on.
func (f *forge) BaselineJobs(ctx context.Context, repo string, run cipoll.Run, wantTimings bool) (cipoll.Baseline, error) {
	prev, err := f.fj.GetLatestFinishedRun(ctx, repo, run.WorkflowKey, run.HeadBranch)
	if err != nil {
		return cipoll.Baseline{}, fmt.Errorf("prior-run lookup: %w", err)
	}
	if prev == nil {
		return cipoll.Baseline{}, nil // no prior run to seed from
	}

	var jobs []fjclient.Job
	if wantTimings {
		jobs, err = f.fj.GetFinishedJobs(ctx, repo, prev.ID, prev.IndexInRepo)
	} else {
		jobs, err = f.fj.GetJobs(ctx, repo, prev.ID)
	}
	if err != nil {
		return cipoll.Baseline{}, fmt.Errorf("jobs for prior run %d: %w", prev.ID, err)
	}
	return cipoll.Baseline{Jobs: toCIJobs(jobs), RunID: prev.ID}, nil
}

// Outcome maps a terminal run to its final card state and accent color.
//
// Forgejo folds GitHub's status and conclusion into one field, so there is no
// empty-conclusion case to fall back from; anyFailed only covers a run that
// reports an unrecognised terminal status while a job under it failed.
func (f *forge) Outcome(run cipoll.Run, anyFailed bool) (state, color string) {
	switch run.Conclusion {
	case ci.ConclusionSuccess:
		return cipoll.OutcomeSuccess, pushward.ColorGreen
	case ci.ConclusionFailure:
		return cipoll.OutcomeFailed, pushward.ColorRed
	case ci.ConclusionCancelled:
		return cipoll.OutcomeCancelled, pushward.ColorOrange
	case ci.ConclusionSkipped:
		return cipoll.OutcomeSkipped, pushward.ColorBlue
	}
	if anyFailed {
		return cipoll.OutcomeFailed, pushward.ColorRed
	}
	return cipoll.OutcomeComplete, pushward.ColorGreen
}

// toRun converts a normalized run into the shared poller's shape. The client has
// already split Forgejo's status field and parsed the timestamps, so the only
// work left is resolving the repo link.
func (f *forge) toRun(repo string, r fjclient.Run) cipoll.Run {
	return cipoll.Run{
		ID:          r.ID,
		Number:      r.IndexInRepo,
		Name:        r.Name,
		WorkflowKey: r.WorkflowID,
		Status:      r.Status,
		Conclusion:  r.Conclusion,
		RawStatus:   r.RawStatus,
		HeadBranch:  r.HeadBranch,
		Event:       r.Event,
		CreatedAt:   r.CreatedAt,
		// The API's own html_url, never one built locally: Forgejo derives it from
		// the run's index_in_repo, not the id this bridge fetches by.
		HTMLURL: r.HTMLURL,
		RepoURL: f.repoURL(repo, r),
	}
}

// repoURL is the card's secondary link. Forgejo embeds the repository in a run,
// but the field is optional across versions, so fall back to composing it from
// the configured instance root rather than emitting an empty link.
func (f *forge) repoURL(repo string, run fjclient.Run) string {
	if run.RepoHTMLURL != "" {
		return run.RepoHTMLURL
	}
	return f.fj.WebBase() + "/" + repo
}
