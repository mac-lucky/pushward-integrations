package forgejo

import "time"

// Run is a Forgejo action run, normalized into the shape the github bridge's
// poller was written against: the status/conclusion split synthesised,
// timestamps parsed, a display name resolved.
//
// Keeping the translation at the client boundary is what lets the poller stay a
// near-verbatim copy of github's, and what would make a later lift of the
// orchestration into a shared package a pure move.
type Run struct {
	// ID is the /runs/{run_id} path parameter.
	ID int64
	// IndexInRepo is the number the UI shows and the one html_url is built from.
	// It equals a task's run_number. It is never a path parameter.
	IndexInRepo int64
	// Name is the workflow filename minus its extension ("tofu.yml" -> "tofu").
	// Forgejo has no workflow display name to offer.
	Name string
	// WorkflowID is the raw filename, and the runs `workflow_id` filter value.
	WorkflowID string
	// Title is the commit subject, truncated. A log field - it is far too long
	// for the activity subtitle.
	Title string

	Status     string // ci.StatusQueued | ci.StatusInProgress | ci.StatusCompleted
	Conclusion string // "" | ci.ConclusionSuccess | Failure | Cancelled | Skipped
	RawStatus  string // Forgejo's own value, kept for logs

	HeadBranch string // the bare prettyref; see fullRef before querying with it
	HeadSHA    string
	Event      string // trigger_event, falling back to event

	CreatedAt time.Time
	UpdatedAt time.Time
	StartedAt time.Time
	StoppedAt time.Time

	// HTMLURL is always the API's own value. It is built from IndexInRepo rather
	// than ID, so anything constructed locally points at a different run.
	HTMLURL string

	RepoFullName string
	RepoHTMLURL  string
	NeedApproval bool

	// Duration is only meaningful once the run is terminal.
	Duration time.Duration
}

// Terminal reports whether the run has stopped.
func (r Run) Terminal() bool { return isTerminal(r.RawStatus) }

// Job is one action job, normalized.
//
// StartedAt and CompletedAt come from the matching /actions/tasks row and stay
// zero when that join misses, which the shared ladder reads as "unknown" exactly
// as it reads an unstamped GitHub job.
type Job struct {
	ID     int64
	RunID  int64
	TaskID int64 // the /actions/tasks join key

	Name       string
	Status     string
	Conclusion string
	RawStatus  string
	Needs      []string

	StartedAt   time.Time
	CompletedAt time.Time
}

// Repository is a repo the token can see.
type Repository struct {
	FullName string
	HTMLURL  string
	Archived bool
	Empty    bool
}
