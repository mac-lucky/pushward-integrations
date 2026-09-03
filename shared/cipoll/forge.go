// Package cipoll polls a CI forge and renders each run as a PushWard steps
// Live Activity.
//
// It owns everything that is not forge-specific: the poll loop, the per-repo
// tracked-run state machine, the total-steps clamp, redundant-tick suppression,
// the live-progress anchoring and the two-phase end. A forge plugs in through
// Forge, which is the whole seam - two bridges consume this today (github and
// forgejo) and a third needs only an adapter.
//
// The vocabulary is GitHub Actions', matching shared/ci for the same reason:
// most forges either speak it already or translate once at their client
// boundary.
package cipoll

import (
	"context"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/ci"
)

// Run is one CI run, forge-neutral.
//
// Every URL field is the forge's own value rather than something composed here:
// Forgejo builds a run's page from a display index that is not the id the API is
// queried by, so a locally built link points at a different run.
type Run struct {
	// ID is the {run_id} path parameter the forge is queried by.
	ID int64
	// Number is the run number a user sees, when the forge has one distinct from
	// ID. Logs only; zero means "same as ID, or none".
	Number int64
	// Name is the workflow's display name, or its filename stem where the forge
	// offers no display name.
	Name string
	// WorkflowKey identifies the workflow definition, stably across runs, and is
	// what BaselineJobs looks a prior run up by. Empty means the forge could not
	// supply one, which short-circuits the step seed to the live scan.
	WorkflowKey string

	Status     string // ci.StatusQueued | ci.StatusInProgress | ci.StatusCompleted
	Conclusion string // "" until terminal, then a ci.Conclusion* value
	RawStatus  string // the forge's own status value, kept for logs

	HeadBranch string
	Event      string // logs only; empty where the forge does not report it

	CreatedAt time.Time

	// HTMLURL is the run's page, and RepoURL the card's secondary link. Both
	// come from the adapter so a forge with an unusual URL scheme resolves them
	// itself.
	HTMLURL string
	RepoURL string
}

// Terminal reports whether the run has stopped. The status vocabulary is
// GitHub's, so this is the single check both forges reduce to: Forgejo's own
// terminal statuses are exactly the ones its client normalizes to
// ci.StatusCompleted.
func (r Run) Terminal() bool { return r.Status == ci.StatusCompleted }

// Card state text, shared so two forges cannot drift apart on the words a user
// reads. Which of them a run maps to is per-forge (see Forge.Outcome); the
// vocabulary is not.
const (
	OutcomeSuccess   = "Success"
	OutcomeFailed    = "Failed"
	OutcomeCancelled = "Cancelled"
	OutcomeSkipped   = "Skipped"
	OutcomeComplete  = "Complete"
)

// Baseline is a prior finished run of the same workflow, used to seed a stable
// total-steps denominator and the per-group durations. The zero value means
// there is no usable prior run, which is not an error.
type Baseline struct {
	Jobs []ci.Job
	// RunID identifies the run the jobs came from. Logs only.
	RunID int64
	// Duration is how long the prior run itself took, from the run object rather
	// than from its jobs, and zero when the forge does not say. See
	// ci.BaselineWeights for what it bounds and, failing everything else, seeds.
	Duration time.Duration
}

// Forge is one CI forge, as much of it as the poller needs.
//
// Implementations own their wire format and their URL construction. Errors are
// returned, not logged: the loop's response to a failed lookup is uniform (skip
// this repo, or fall back to the live scan, and try again next tick) and it logs
// once at the point it makes that decision, so an implementation that logged too
// would double up.
//
// Every method returning a pointer or slice must return it non-nil when it
// returns a nil error. The loop dereferences what it is handed.
type Forge interface {
	// ListRepos returns every repo under owner that the credentials can reach.
	// This one does surface its error: it runs before any run is tracked, and
	// whether a failure is fatal is the caller's policy (Options.DiscoveryRequired).
	ListRepos(ctx context.Context, owner string) ([]string, error)

	// ActiveRuns returns the repo's runs that are queued or in progress. A forge
	// filters out statuses that may never execute - a run held for approval
	// would otherwise put a card on the lock screen that only the lifetime guard
	// could reclaim.
	ActiveRuns(ctx context.Context, repo string) ([]Run, error)

	// GetRun re-reads one run. The loop calls it to confirm the run itself is
	// terminal before ending the activity, because every forge here creates jobs
	// lazily and a poll landing between job waves sees all *visible* jobs done.
	//
	// Must be non-nil when the error is nil. Mapping a 404 to (nil, nil) is an
	// ordinary Go idiom and this codebase uses it elsewhere, so it is worth
	// saying: here it would crash the loop.
	GetRun(ctx context.Context, repo string, runID int64) (*Run, error)

	// LiveJobs returns the run's current jobs, already converted for the ladder.
	LiveJobs(ctx context.Context, repo string, runID int64) ([]ci.Job, error)

	// BaselineJobs returns the workflow's most recent finished run on ref, or on
	// any ref when ref is blank, to seed a stable total-steps denominator from
	// frame one. The loop drives the widening - the run's own ref first, then any
	// ref, since a tag build or a fresh branch has no earlier run of its own -
	// and the forge owns which finished run on that ref counts (a successful one
	// that ran the whole DAG, failing that any terminal one). ref is the run's
	// HeadBranch as the forge reported it; the adapter qualifies it. A zero
	// Baseline means there is no usable run on that ref, which is not an error.
	//
	// wantTimings says whether the caller will read per-group durations off the
	// result; a forge whose job objects carry no timestamps can then skip the
	// extra lookup that fills them in.
	BaselineJobs(ctx context.Context, repo string, run Run, ref string, wantTimings bool) (Baseline, error)

	// Outcome maps a terminal run to the card's final state text and accent
	// color. Forges differ here deliberately - one collapses everything to
	// success or failure, another distinguishes cancelled and skipped - so this
	// stays adapter-owned rather than being unified into the loop.
	//
	// anyFailed is the ladder's view of the run's jobs, for the case where the
	// run reports no usable conclusion of its own.
	Outcome(run Run, anyFailed bool) (state, color string)

	// Budget reports how many requests are left in the current window and when it
	// refills. It is what the loop paces detection against: the per-repo sweep
	// stretches to fit what is left, and discovery is dropped before the allowance
	// runs out.
	//
	// ok is false when there is nothing to pace against - a forge that publishes no
	// rate-limit headers at all (every self-hosted Forgejo), or one that has not
	// seen a response yet. That is a complete answer, and leaves the loop on its
	// configured intervals.
	//
	// On Forge rather than an optional interface the adapter may or may not
	// satisfy: a forge that failed to provide this would disable every pacing
	// decision with no signal at all, indistinguishable from a forge that
	// legitimately has no budget. One stub line is cheaper than that failure mode,
	// and the compiler checks it.
	//
	// Deliberately not error-returning, and must not itself make a request: the
	// loop asks on every tick.
	Budget() (remaining int, resetAt time.Time, ok bool)
}
