package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"time"
)

const (
	// taskPageSize is the documented maximum page size.
	taskPageSize = 50

	// maxTaskPages bounds the historic walk. The task list is repo-wide and
	// newest-first, so a recent run's rows are almost always on page 1; this caps
	// the damage when they are not.
	maxTaskPages = 3
)

// listTasks fetches one page of the repo's action tasks.
//
// Despite the endpoint's name and its "workflow_runs" JSON key, each row is one
// JOB, and its id is the value a job carries as task_id. This is the only place
// per-job timing exists in the Forgejo API.
//
// GOTCHA: `limit` on its own is IGNORED here - the endpoint answers with every
// row it has. Both parameters must be sent, which is why this always sets page.
func (c *Client) listTasks(ctx context.Context, repo string, page int, statuses ...string) ([]wireTask, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("page", strconv.Itoa(page))
	q.Set("limit", strconv.Itoa(taskPageSize))
	for _, s := range statuses {
		q.Add("status", s)
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/tasks?%s",
		c.apiBase, url.PathEscape(owner), url.PathEscape(name), q.Encode())

	body, err := c.doWithRetry(ctx, endpoint, "list tasks")
	if err != nil {
		return nil, err
	}
	var resp wireTasksResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode tasks: %w", err)
	}
	return resp.Tasks, nil
}

// stampLiveTimings fills StartedAt on the running jobs from the repo's running
// tasks.
//
// The jobs endpoint carries no timestamps at all, so this join is the only
// source of a step's start - the anchor the live-progress window is measured
// from. Filtering on status=running keeps the response to the handful of rows
// that matter rather than the repo's whole task history.
//
// A failure here is not a poll failure: the jobs come back unstamped, the ladder
// reads that as "unknown" and renders the static bar.
func (c *Client) stampLiveTimings(ctx context.Context, repo string, jobs []Job) []Job {
	if !c.opts.LiveTimings || !anyRunning(jobs) {
		return jobs
	}
	tasks, err := c.listTasks(ctx, repo, 1, StatusRunning)
	if err != nil {
		slog.Warn("live task timings unavailable, falling back to the static bar",
			"repo", repo, "error", err)
		return jobs
	}
	joinTasks(jobs, tasks, 0)
	return jobs
}

// stampHistoricTimings fills StartedAt and CompletedAt on a finished run's jobs
// by paging the repo's task list until every task_id is matched.
//
// indexInRepo is the run's UI number, which is what a task's run_number carries;
// it is used only to log a mismatch. The join itself is by task id, which is
// unique repo-wide and therefore authoritative on its own.
//
// Partial results are deliberate: an unmatched job stays unmeasured, and
// ci.GroupWeights floors that group into a thin pill rather than dropping it.
func (c *Client) stampHistoricTimings(ctx context.Context, repo string, jobs []Job, indexInRepo int64) []Job {
	if !c.opts.HistoryTimings {
		return jobs
	}
	want := 0
	for _, j := range jobs {
		if j.TaskID != 0 {
			want++
		}
	}
	if want == 0 {
		return jobs
	}

	matched := 0
	for page := 1; page <= maxTaskPages && matched < want; page++ {
		tasks, err := c.listTasks(ctx, repo, page)
		if err != nil {
			slog.Warn("historic task timings unavailable, pills will be equal-width",
				"repo", repo, "page", page, "error", err)
			break
		}
		if len(tasks) == 0 {
			break
		}
		matched += joinTasks(jobs, tasks, indexInRepo)
		if len(tasks) < taskPageSize {
			break
		}
	}
	if matched < want {
		slog.Debug("some jobs had no matching task row",
			"repo", repo, "run", indexInRepo, "matched", matched, "want", want)
	}
	return jobs
}

// joinTasks copies timings onto the jobs whose task_id matches a task id, and
// reports how many it stamped.
//
// A terminal task's span is run_started_at..updated_at. A running task has only
// a start, so CompletedAt is left zero and the group stays unmeasured for
// weighting while still being anchorable.
func joinTasks(jobs []Job, tasks []wireTask, indexInRepo int64) int {
	if len(jobs) == 0 || len(tasks) == 0 {
		return 0
	}
	byID := make(map[int64]wireTask, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}

	matched := 0
	for i := range jobs {
		t, ok := byID[jobs[i].TaskID]
		if !ok {
			continue
		}
		if indexInRepo != 0 && t.RunNumber != 0 && t.RunNumber != indexInRepo {
			// Task ids are unique repo-wide, so the id join wins; this only ever
			// signals that the run number was not what we assumed.
			slog.Debug("task run_number does not match the run",
				"task", t.ID, "task_run_number", t.RunNumber, "run", indexInRepo)
		}
		matched++
		jobs[i].StartedAt = t.RunStartedAt.Time()
		if isTerminal(t.Status) {
			jobs[i].CompletedAt = t.UpdatedAt.Time()
		}
	}
	return matched
}

func anyRunning(jobs []Job) bool {
	for _, j := range jobs {
		if j.RawStatus == StatusRunning {
			return true
		}
	}
	return false
}

// Duration is a job's measured wall-clock span, or 0 when the join left it
// unmeasured. Exposed for logging; the ladder computes its own.
func (j Job) Duration() time.Duration {
	if j.StartedAt.IsZero() || j.CompletedAt.IsZero() {
		return 0
	}
	if d := j.CompletedAt.Sub(j.StartedAt); d > 0 {
		return d
	}
	return 0
}
