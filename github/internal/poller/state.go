package poller

import "time"

type trackedRun struct {
	Repo       string
	RunID      int64
	Name       string
	Branch     string
	Slug       string
	HTMLURL    string
	StartedAt  time.Time
	LastUpdate time.Time
	endTimer   *time.Timer

	// maxTotalSteps tracks the highest TotalSteps seen across polls.
	// GitHub lazily creates jobs behind unsatisfied needs/if conditions,
	// so new steps appear as the workflow progresses. We never decrease
	// the total to avoid confusing step jumps (e.g. 1/5 → 5/6 → 6/7).
	maxTotalSteps int
	maxStepRows   []int
}
