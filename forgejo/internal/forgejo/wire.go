package forgejo

import (
	"path"
	"strings"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// maxTitleLen bounds the commit subject carried on a Run. It is a log field
// only - the activity subtitle uses the workflow name - but an unbounded string
// from a webhook-adjacent field has no business sitting in memory per tracked
// run.
const maxTitleLen = 200

// wireRunsResponse is the envelope of GET /repos/{o}/{r}/actions/runs.
type wireRunsResponse struct {
	TotalCount   int64     `json:"total_count"`
	WorkflowRuns []wireRun `json:"workflow_runs"`
}

// wireRun is one ActionRun.
//
// Deliberately absent: `event_payload`, a multi-KB JSON string that is the main
// reason a 50-row page costs ~300 KB. Go ignores unknown fields, so leaving it
// undeclared costs nothing and modelling it would invite someone to parse it.
type wireRun struct {
	ID int64 `json:"id"`
	// IndexInRepo is the number the UI shows, the number html_url is built from,
	// and the value a task's run_number carries. It is NOT the /runs/{run_id}
	// path parameter - that is ID. Run id 39 lives at .../actions/runs/33.
	IndexInRepo int64  `json:"index_in_repo"`
	Title       string `json:"title"` // commit subject; can be long
	// WorkflowID is a FILENAME ("tofu.yml"), not the int GitHub sends, and is
	// also what the runs `workflow_id` query filter takes.
	WorkflowID string `json:"workflow_id"`
	// PrettyRef is the BARE ref ("master"). The `ref` query filter needs the full
	// ref; see fullRef.
	PrettyRef string `json:"prettyref"`
	CommitSHA string `json:"commit_sha"`
	Event     string `json:"event"` // frequently the empty string
	// TriggerEvent is the reliable one: push / schedule / workflow_dispatch.
	TriggerEvent string `json:"trigger_event"`
	// Status is the ONLY outcome field. A Forgejo run has no `conclusion`.
	Status            string          `json:"status"`
	Started           flexTime        `json:"started"`
	Stopped           flexTime        `json:"stopped"`
	Created           flexTime        `json:"created"`
	Updated           flexTime        `json:"updated"`
	Duration          int64           `json:"duration"` // NANOSECONDS
	HTMLURL           string          `json:"html_url"` // never reconstruct this
	NeedApproval      bool            `json:"need_approval"`
	IsForkPullRequest bool            `json:"is_fork_pull_request"`
	Repository        *wireRepository `json:"repository"`
	TriggerUser       *wireUser       `json:"trigger_user"`
}

type wireRepository struct {
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
	Archived bool   `json:"archived"`
	Empty    bool   `json:"empty"`
}

type wireUser struct {
	Login string `json:"login"`
}

// wireJob is one ActionRunJob from GET .../runs/{id}/jobs, which returns a BARE
// ARRAY - no envelope, no pagination parameters. It carries no timestamps, no
// conclusion and no steps[]; TaskID is the join key into /actions/tasks, which
// is the only place per-job timing exists.
type wireJob struct {
	ID      int64    `json:"id"`
	RunID   int64    `json:"run_id"`
	Attempt int64    `json:"attempt"`
	Handle  string   `json:"handle"`
	RepoID  int64    `json:"repo_id"`
	OwnerID int64    `json:"owner_id"`
	Name    string   `json:"name"`
	Needs   []string `json:"needs"` // may be null
	RunsOn  []string `json:"runs_on"`
	TaskID  int64    `json:"task_id"`
	Status  string   `json:"status"`
}

// wireTasksResponse is GET /repos/{o}/{r}/actions/tasks. Its rows are keyed
// "workflow_runs" in the JSON but each one is a per-JOB task, which is why this
// has its own type rather than reusing wireRunsResponse - decoding tasks as runs
// succeeds and yields nonsense.
type wireTasksResponse struct {
	TotalCount int64      `json:"total_count"`
	Tasks      []wireTask `json:"workflow_runs"`
}

type wireTask struct {
	ID           int64    `json:"id"`   // == the job's task_id: the join key
	Name         string   `json:"name"` // the JOB name, matrix parens included
	HeadBranch   string   `json:"head_branch"`
	HeadSHA      string   `json:"head_sha"`
	RunNumber    int64    `json:"run_number"` // == the run's index_in_repo
	Event        string   `json:"event"`
	DisplayTitle string   `json:"display_title"`
	Status       string   `json:"status"`
	WorkflowID   string   `json:"workflow_id"`
	URL          string   `json:"url"`
	CreatedAt    flexTime `json:"created_at"`
	RunStartedAt flexTime `json:"run_started_at"`
	UpdatedAt    flexTime `json:"updated_at"`
}

type wireVersion struct {
	Version string `json:"version"`
}

// toRun normalizes a wire run into the shape the poller consumes.
func toRun(w wireRun) Run {
	status, conclusion := normalizeStatus(w.Status)

	event := w.TriggerEvent
	if event == "" {
		event = w.Event
	}

	r := Run{
		ID:           w.ID,
		IndexInRepo:  w.IndexInRepo,
		Name:         workflowDisplayName(w.WorkflowID, w.Title),
		WorkflowID:   w.WorkflowID,
		Title:        text.Truncate(w.Title, maxTitleLen),
		Status:       status,
		Conclusion:   conclusion,
		RawStatus:    w.Status,
		HeadBranch:   w.PrettyRef,
		HeadSHA:      w.CommitSHA,
		Event:        event,
		CreatedAt:    w.Created.Time(),
		UpdatedAt:    w.Updated.Time(),
		StartedAt:    w.Started.Time(),
		StoppedAt:    w.Stopped.Time(),
		HTMLURL:      w.HTMLURL,
		NeedApproval: w.NeedApproval,
		Duration:     time.Duration(w.Duration),
	}
	if w.Repository != nil {
		r.RepoFullName = w.Repository.FullName
		r.RepoHTMLURL = w.Repository.HTMLURL
	}
	return r
}

// toJob normalizes a wire job. Timestamps stay zero here: the jobs endpoint has
// none to give, and the tasks join fills them in afterwards when it can.
func toJob(w wireJob) Job {
	status, conclusion := normalizeStatus(w.Status)
	return Job{
		ID:         w.ID,
		RunID:      w.RunID,
		TaskID:     w.TaskID,
		Name:       w.Name,
		Status:     status,
		Conclusion: conclusion,
		RawStatus:  w.Status,
		Needs:      w.Needs,
	}
}

// workflowDisplayName prettifies a workflow filename for the activity subtitle.
// Forgejo exposes no workflow display name at all - only the filename - so
// "tofu.yml" becomes "tofu". An empty workflow_id falls back to the run title,
// then to a generic label.
func workflowDisplayName(workflowID, title string) string {
	name := path.Base(strings.TrimSpace(workflowID))
	if name != "" && name != "." && name != "/" {
		if ext := path.Ext(name); ext == ".yml" || ext == ".yaml" {
			name = strings.TrimSuffix(name, ext)
		}
		if name != "" {
			return name
		}
	}
	if t := strings.TrimSpace(title); t != "" {
		return text.Truncate(t, 60)
	}
	return "workflow"
}
