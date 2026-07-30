package github

import "time"

type WorkflowRunsResponse struct {
	TotalCount   int           `json:"total_count"`
	WorkflowRuns []WorkflowRun `json:"workflow_runs"`
}

type WorkflowRun struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	HeadBranch string    `json:"head_branch"`
	// WorkflowID identifies the workflow definition this run belongs to. It is
	// stable across runs of the same workflow, letting us look up a prior run's
	// full step shape to seed a stable total-steps denominator.
	WorkflowID int64  `json:"workflow_id"`
	HTMLURL    string `json:"html_url"`
}

// WorkflowsResponse is the subset of GET /repos/{o}/{r}/actions/workflows the
// bridge reads: only whether the repo defines any workflows at all. The list
// itself is never used, so the request asks for one entry.
type WorkflowsResponse struct {
	TotalCount int `json:"total_count"`
}

type JobsResponse struct {
	TotalCount int   `json:"total_count"`
	Jobs       []Job `json:"jobs"`
}

type Job struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	StartedAt  string `json:"started_at"`
	// CompletedAt is set once the job finishes (empty while queued/in_progress).
	// Paired with StartedAt it yields the job's wall-clock duration, used to size
	// the step pills from a prior finished run.
	CompletedAt string `json:"completed_at"`
	Steps       []Step `json:"steps"`
}

type Step struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	Number      int    `json:"number"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
}

type Repository struct {
	FullName string `json:"full_name"`
	Archived bool   `json:"archived"`
	Disabled bool   `json:"disabled"`
}

// User is the subset of GET /user used to resolve the token's own login so
// repo discovery can choose the correct endpoint for owner.
type User struct {
	Login string `json:"login"`
}
