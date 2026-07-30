package forgejo

import "github.com/mac-lucky/pushward-integrations/shared/ci"

// Forgejo's action status enum. Unlike GitHub there is no separate conclusion
// field: one value carries both "how far along" and "how it went".
const (
	StatusUnknown   = "unknown"
	StatusWaiting   = "waiting"
	StatusRunning   = "running"
	StatusSuccess   = "success"
	StatusFailure   = "failure"
	StatusCancelled = "cancelled"
	StatusSkipped   = "skipped"
	StatusBlocked   = "blocked"
)

// activeStatuses is the idle probe's filter.
//
// "blocked" is deliberately excluded. A run held for approval may never execute,
// and tracking it would put a Live Activity on the lock screen that only the
// 12-hour lifetime guard could reclaim.
var activeStatuses = []string{StatusRunning, StatusWaiting}

// finishedStatusSets orders the prior-run lookups used to seed a stable step
// total. A fully-successful run executed the whole job DAG, so its group count
// is the most accurate seed; only if there is none do we accept a run that
// stopped early.
//
// GitHub could express the fallback as the single umbrella value "completed".
// Forgejo has no such value - passing one is an HTTP 400 - so the second pass
// enumerates the terminal statuses instead, which its repeatable `status` array
// parameter accepts in one request.
var finishedStatusSets = [][]string{
	{StatusSuccess},
	{StatusFailure, StatusCancelled, StatusSkipped},
}

// normalizeStatus splits Forgejo's single status field into the GitHub-shaped
// status/conclusion pair the shared ladder consumes.
//
// An unrecognised value maps to queued rather than completed on purpose:
// over-reporting completion would end a Live Activity while its run was still
// going, whereas over-reporting pending only defers the end by one poll.
func normalizeStatus(s string) (status, conclusion string) {
	switch s {
	case StatusSuccess:
		return ci.StatusCompleted, ci.ConclusionSuccess
	case StatusFailure:
		return ci.StatusCompleted, ci.ConclusionFailure
	case StatusCancelled:
		return ci.StatusCompleted, ci.ConclusionCancelled
	case StatusSkipped:
		return ci.StatusCompleted, ci.ConclusionSkipped
	case StatusRunning:
		return ci.StatusInProgress, ""
	default: // waiting, blocked, unknown, and anything a later release adds
		return ci.StatusQueued, ""
	}
}

// isTerminal reports whether a Forgejo status means the run or task has stopped.
func isTerminal(s string) bool {
	switch s {
	case StatusSuccess, StatusFailure, StatusCancelled, StatusSkipped:
		return true
	}
	return false
}
