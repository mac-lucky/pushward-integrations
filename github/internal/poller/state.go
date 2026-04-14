package poller

import "time"

// timerPair holds both phase-1 and phase-2 timers so they can both be cancelled.
type timerPair struct {
	phase1 *time.Timer
	phase2 *time.Timer
}

type trackedRun struct {
	Repo       string
	RunID      int64
	Name       string
	Slug       string
	HTMLURL    string
	LastUpdate time.Time
	endTimers  *timerPair

	// maxTotalSteps tracks the highest TotalSteps seen across polls.
	// GitHub lazily creates jobs behind unsatisfied needs/if conditions,
	// so new steps appear as the workflow progresses. We never decrease
	// the total to avoid confusing step jumps (e.g. 1/5 → 5/6 → 6/7).
	maxTotalSteps int
	maxStepRows   []int
	maxStepLabels []string
}
