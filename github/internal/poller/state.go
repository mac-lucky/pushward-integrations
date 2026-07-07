package poller

import (
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/syncx"
)

type trackedRun struct {
	Repo       string
	RunID      int64
	Name       string
	Slug       string
	HTMLURL    string
	LastUpdate time.Time
	trackedAt  time.Time // when this run was first tracked; bounds absolute lifetime
	// endTimers is non-nil once a two-phase end is pending. The TimerGroup
	// holds the current phase timer (phase 1, then phase 2) and lets shutdown
	// drain in-flight end deliveries (Stop/Close + Wait).
	endTimers *syncx.TimerGroup

	// maxTotalSteps tracks the highest TotalSteps seen across polls.
	// GitHub lazily creates jobs behind unsatisfied needs/if conditions,
	// so new steps appear as the workflow progresses. We never decrease
	// the total to avoid confusing step jumps (e.g. 1/5 → 5/6 → 6/7).
	maxTotalSteps int
	maxStepRows   []int
	maxStepLabels []string
	maxStepColors []string
	// stepWeightByName sizes the pills from the prior run's per-group durations,
	// keyed by group label. Historical (never recomputed from the live,
	// in-progress jobs) and read-only after seeding; projected onto the current
	// step_labels at send time, so a weight always tracks its own label even if
	// GitHub reveals the groups in a different order. Nil means no usable prior
	// run — callers then omit step_weights and pills render equal-width.
	stepWeightByName map[string]float64
	// shapeSent is the maxTotalSteps value at the time we last included
	// step_rows/step_labels/step_weights in a merge-patch. When unchanged across
	// polls we skip those slices to keep the tick payload minimal.
	shapeSent int

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
}
