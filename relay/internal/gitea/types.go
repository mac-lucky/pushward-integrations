package gitea

// Gitea and Forgejo both emit Actions webhooks, but with different shapes, so
// this package serves two routes from one config:
//   - Gitea (1.24+) sends GitHub-parity workflow_run / workflow_job events with
//     per-job granularity, driving a live build-progress Live Activity (steps).
//   - Forgejo sends only terminal action_run_success / action_run_failure /
//     action_run_recover events, driving a completion-result activity (generic).
//
// Routes are discriminated by body shape rather than the vendor event header:
// the Gitea handler branches on which of workflow_run / workflow_job is present,
// and the Forgejo handler branches on the bare action value.

// giteaPayload is the Gitea workflow_run / workflow_job webhook body. Both event
// types share the top-level action and repository fields; the presence of the
// workflow_run or workflow_job object selects the event. Gitea does not populate
// a name on workflow_run (only display_title), so the workflow name comes from
// the workflow object.
type giteaPayload struct {
	Action      string       `json:"action"`
	Workflow    *workflow    `json:"workflow"`
	WorkflowRun *workflowRun `json:"workflow_run"`
	WorkflowJob *workflowJob `json:"workflow_job"`
	Repository  *repository  `json:"repository"`
}

type workflow struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type workflowRun struct {
	ID           int64  `json:"id"`
	DisplayTitle string `json:"display_title"`
	HeadBranch   string `json:"head_branch"`
	RunNumber    int64  `json:"run_number"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	HTMLURL      string `json:"html_url"`
}

// workflowJob carries one job of a run. The steps[] array is intentionally not
// modeled: no webhook fires on step transitions, so jobs are the finest
// progress granularity available (jobs are aggregated into step groups).
type workflowJob struct {
	ID         int64  `json:"id"`
	RunID      int64  `json:"run_id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

type repository struct {
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
}

// forgejoPayload is the Forgejo action_run_* webhook body. The action value is
// the bare terminal outcome ("success" / "failure" / "recover"), and the run
// object is keyed "run" (not "workflow_run").
type forgejoPayload struct {
	Action string      `json:"action"`
	Run    *forgejoRun `json:"run"`
}

type forgejoRun struct {
	ID         int64       `json:"id"`
	Title      string      `json:"title"`
	WorkflowID string      `json:"workflow_id"`
	PrettyRef  string      `json:"prettyref"`
	HTMLURL    string      `json:"html_url"`
	Repository *repository `json:"repository"`
}

// runRecord tracks a Gitea run in the state store under subKey "run". Completed
// flips true on the workflow_run completed event and stays as a tombstone so
// late job events cannot resurrect an already-ended activity. Group rows are
// bounded by StaleTimeout and cleared wholesale (DeleteGroup) when a newer run
// supersedes the tracked one.
type runRecord struct {
	RunID     int64  `json:"run_id"`
	Workflow  string `json:"workflow"`
	Branch    string `json:"branch"`
	HTMLURL   string `json:"html_url"`
	RepoURL   string `json:"repo_url"`
	Completed bool   `json:"completed"`
}

// jobRecord tracks one Gitea job under subKey "job:<id>".
type jobRecord struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}
