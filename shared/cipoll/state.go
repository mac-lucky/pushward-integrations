package cipoll

import (
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/ci"
	"github.com/mac-lucky/pushward-integrations/shared/syncx"
)

// trackedRun is the per-repo state of the run currently on screen. Keyed by
// repo, not by run: the activity slug is derived from the repo, so one repo can
// only ever show one card and a new run supersedes its predecessor.
type trackedRun struct {
	RunID int64
	// Name is the workflow's display name, kept because the completion frame
	// rebuilds the subtitle without re-reading the run.
	Name    string
	Slug    string
	HTMLURL string
	// RepoURL is the card's secondary link, resolved once at track time by the
	// forge adapter.
	RepoURL string
	// Ref is the head branch as the forge reported it, the key the seed cache
	// files the run under when it finishes, and createdAt is the forge's own
	// creation stamp, from which the run's length is measured then. Both are
	// kept from detection so the completion tick does not depend on what the
	// adapter's re-read fills in.
	Ref       string
	createdAt time.Time
	// startedAt is the start of the attempt on screen, refreshed from the
	// terminal re-read because a run detected while queued had none yet. The
	// end records it next to RunID; see endedRun.
	startedAt  time.Time
	LastUpdate time.Time
	trackedAt  time.Time // when this run was first tracked; bounds absolute lifetime
	// endTimers is non-nil once a two-phase end is pending. The TimerGroup
	// holds the current phase timer (phase 1, then phase 2) and lets shutdown
	// drain in-flight end deliveries (Stop/Close + Wait).
	endTimers *syncx.TimerGroup

	// maxTotalSteps tracks the highest TotalSteps seen across polls. Every forge
	// here lazily creates jobs behind unsatisfied needs/if conditions, so new
	// steps appear as the workflow progresses. We never decrease the total to
	// avoid confusing step jumps (e.g. 1/5 -> 5/6 -> 6/7).
	maxTotalSteps int
	maxStepRows   []int
	maxStepLabels []string
	maxStepColors []string
	// stepWeightByName sizes the pills from the prior run's per-group durations,
	// keyed by group label. An entry is a measurement or a run-duration share; a
	// group with no entry draws at the mean. Historical (never recomputed from
	// the live, in-progress jobs) and read-only after seeding; projected onto
	// the current step_labels at send time, so a weight always tracks its own
	// label even if the forge reveals the groups in a different order. Nil means
	// no usable prior run - callers then send equal weights, see payloadWeights.
	stepWeightByName map[string]float64
	// shapeSent is the step_labels the last landed patch carried the ladder
	// (step_rows/step_labels/step_colors/step_weights) with, nil until one has.
	// The ladder ships again when the tracked labels differ: longer because the
	// forge revealed a group, or the same length because the seed named a group
	// this run does not have. Unchanged across polls, the slices stay off the
	// tick to keep its payload minimal. A bare-total seed has no labels and
	// compares unequal to the first scan's, which is what pays its ladder.
	shapeSent []string

	// Change-detection state for pollActive: a PATCH (and the APNs push it
	// triggers) is sent only when one of these scalars changes or a heartbeat
	// is due, so a run parked on one long step (build wait, integration tests)
	// doesn't emit an identical update to every device every tick. Promoted
	// only after a successful patch so a failed send is re-evaluated next tick.
	lastProgress    float64
	lastState       string
	lastCurrentStep int
	lastTotalSteps  int
	lastPatchAt     time.Time

	// Which group the live-progress window last sent belongs to. Re-anchoring
	// only when it changes is what keeps the bar from snapping back to empty
	// mid-step, and keeps the anchors off the heartbeat patches (moving
	// start_date/end_date makes a push high-priority and skips server-side
	// coalescing). Held by name, not by index: the total-steps clamp can swap in
	// a cached label list, so the same index can come to mean a different group.
	// liveSent stays false until a patch lands, so a failed send re-anchors on
	// the next tick.
	liveStepName string
	liveSent     bool

	// declined is the last "live progress not anchored" line written for this
	// run, so a step that cannot animate is reported once.
	declined declined
}

// endedRun is the attempt the loop last closed on a repo. The start is kept
// alongside the ID because a re-run keeps the ID on both forges, and only the
// start says the run is going again.
type endedRun struct {
	id        int64
	startedAt time.Time
}

// closed reports whether run is the attempt e records rather than a later
// re-run of it.
//
// A run reporting no start is still queued: Forgejo zeroes `started` on a
// re-run until a runner picks it up. It is the closed attempt only when that
// attempt had no start either, which covers a forge that never reports one and
// a run cancelled before it started. A lagging list cannot replay the closed
// attempt as startless: GitHub lists only in-progress runs, every one of them
// stamped, and Forgejo's active list is never answered from a cache.
func (e endedRun) closed(run Run) bool {
	if run.ID != e.id {
		return false
	}
	if run.StartedAt.IsZero() {
		return e.startedAt.IsZero()
	}
	return !run.StartedAt.After(e.startedAt)
}

// declined is one "live progress not anchored" line: the step it was written
// for, and the gate that stopped it animating. Compared as a whole, so a step
// that declines for a new reason is reported again.
type declined struct {
	step string
	why  ci.AnchorDecline
}
