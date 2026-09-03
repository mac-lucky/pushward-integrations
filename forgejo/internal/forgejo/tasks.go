package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"time"
)

const (
	// taskPageSize is the documented maximum page size.
	taskPageSize = 50

	// maxTaskPages bounds the historic walk. The task list is repo-wide and
	// newest-first, so a recent run's rows are almost always on page 1; this caps
	// the damage when they are not.
	//
	// The extra pages only cost anything on the runs that would otherwise come back
	// short, since the walk stops as soon as every task_id is matched - and such a
	// run loses its duration-sized pills and its live ETA together.
	//
	// A page count is a proxy for the real question, "have we walked past the run
	// yet?", because the endpoint takes no run filter to ask directly (see the
	// README's API notes). So a run whose task row was pruned can never satisfy
	// matched == want and pays the full cap on every discovery; the Warn below is
	// what makes that visible.
	maxTaskPages = 6
)

// listTasks fetches one page of the repo's action tasks.
//
// Despite the endpoint's name and its "workflow_runs" JSON key, each row is one
// JOB, and its id is the value a job carries as task_id. This is the only place
// per-job timing exists in the Forgejo API.
//
// GOTCHA: `limit` on its own is IGNORED here - the endpoint answers with every
// row it has. Both parameters must be sent, which is why this always sets page.
// The endpoint also takes a status filter; nothing here wants one, since the
// finished rows are as useful as the running ones (see stampLiveTimings).
func (c *Client) listTasks(ctx context.Context, repo string, page int) ([]wireTask, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("page", strconv.Itoa(page))
	q.Set("limit", strconv.Itoa(taskPageSize))
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

// stampLiveTimings fills the timings of a run in progress from the newest page
// of the repo's tasks.
//
// The jobs endpoint carries no timestamps at all, so this join is the only
// source of a step's start - the anchor the live-progress window is measured
// from. The page is taken unfiltered on purpose: a group's start is its FIRST
// shard's start, which may already have finished by the time a poll lands, and
// the completions stamped on the run's last tick are what the next run of the
// workflow is seeded from (see cipoll's shapeCache). Page 1 is the repo's fifty
// newest rows, which is every row of a run in flight unless another run in the
// same repo has since produced more than that.
//
// The lookup runs while something is running and once everything has finished;
// a run parked between job waves has nothing on the page worth a request.
//
// A failure here is not a poll failure: the jobs come back unstamped, the ladder
// reads that as "unknown" and renders the static bar.
func (c *Client) stampLiveTimings(ctx context.Context, repo string, jobs []Job) []Job {
	if !c.opts.LiveTimings || (!anyRunning(jobs) && !allTerminal(jobs)) {
		return jobs
	}
	tasks, err := c.listTasks(ctx, repo, 1)
	if err != nil {
		slog.Warn("live task timings unavailable, falling back to the static bar",
			"repo", repo, "error", err)
		return jobs
	}
	joinTasks(jobs, tasks, 0, time.Time{})
	return jobs
}

// stampHistoricTimings fills StartedAt and CompletedAt on a finished run's jobs
// by paging the repo's task list until every task_id is matched.
//
// indexInRepo is the run's UI number, which is what a task's run_number carries;
// it is used only to log a mismatch. The join itself is by task id, which is
// unique repo-wide and therefore authoritative on its own. stoppedAt is when the
// run itself stopped, the bound on what a row's updated_at may claim.
//
// Partial results are deliberate: an unmatched job stays unmeasured, and
// ci.GroupWeights floors that group into a thin pill rather than dropping it.
func (c *Client) stampHistoricTimings(ctx context.Context, repo string, jobs []Job, indexInRepo int64, stoppedAt time.Time) []Job {
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

	matched, rewritten := 0, 0
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
		m, r := joinTasks(jobs, tasks, indexInRepo, stoppedAt)
		matched, rewritten = matched+m, rewritten+r
		if len(tasks) < taskPageSize {
			break
		}
	}
	if matched < want {
		// Warn, not Debug: these groups keep a floored pill for the whole run, and
		// a run that matches nothing loses its live ETA outright. At the deployed
		// log level a Debug line made that indistinguishable from a pipeline that
		// is simply too short to animate.
		slog.Warn("some jobs had no matching task row, they stay unmeasured",
			"repo", repo, "run", indexInRepo, "want", want,
			"jobs", unmatchedNames(jobs))
	}
	if rewritten > 0 {
		// Same reasoning: the rows were found, but what they say about completion
		// is a later edit, and the groups behind them fall back to the mean.
		slog.Warn("task rows were rewritten after the run, their durations are unknown",
			"repo", repo, "run", indexInRepo, "rewritten", rewritten, "of", matched)
	}
	return jobs
}

// unmatchedNames lists the jobs the tasks walk never found a row for.
//
// The test is the START stamp, deliberately NOT Duration() == 0. Duration is also
// zero for a job whose row was found but is still running, and for one that
// finished inside a second - both routine, and naming them here would make this
// warn fire on healthy repos full of fast jobs, which is the noise the promotion
// from Debug was meant to avoid. joinTasks sets StartedAt on every row it
// matches, so a zero start means no row was found. (A matched-but-unstarted task
// carries the epoch, which reads as zero; it is genuinely unmeasured, so listing
// it is right even though it was technically matched.)
func unmatchedNames(jobs []Job) []string {
	var out []string
	for _, j := range jobs {
		if j.TaskID != 0 && j.StartedAt.IsZero() {
			out = append(out, j.Name)
		}
	}
	return out
}

// stoppedSlack is how far past the run's own stop a task row's updated_at may
// sit and still be read as the task's completion. Forgejo writes the run's stop
// after the last task's, so a genuine completion is never later; a row edited
// after the fact is, by days.
const stoppedSlack = 60 * time.Second

// joinTasks copies timings onto the jobs whose task_id matches a task id, and
// reports how many it stamped and how many of those had a completion it refused.
//
// A task's span is run_started_at..updated_at, but updated_at is a modification
// time, not a stop time: Forgejo rewrites finished rows days after the run (see
// the README's API notes). So with the run's own stop known, a completion later
// than stoppedAt plus stoppedSlack is not believed: the job keeps its start,
// which still says when its group began, and loses its completion, so the group
// falls back to the mean like any other unmeasured one. A running task, or a
// zero stoppedAt (a run in flight, a stop that failed to decode), is taken as
// given.
func joinTasks(jobs []Job, tasks []wireTask, indexInRepo int64, stoppedAt time.Time) (matched, rewritten int) {
	if len(jobs) == 0 || len(tasks) == 0 {
		return 0, 0
	}
	byID := make(map[int64]wireTask, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}

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
		if !isTerminal(t.Status) {
			continue
		}
		end := t.UpdatedAt.Time()
		if !stoppedAt.IsZero() && end.After(stoppedAt.Add(stoppedSlack)) {
			rewritten++
			continue
		}
		jobs[i].CompletedAt = end
	}
	return matched, rewritten
}

func anyRunning(jobs []Job) bool {
	return slices.ContainsFunc(jobs, func(j Job) bool { return j.RawStatus == StatusRunning })
}

func allTerminal(jobs []Job) bool {
	return !slices.ContainsFunc(jobs, func(j Job) bool { return !isTerminal(j.RawStatus) })
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
