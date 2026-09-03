package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/ci"
)

func runningJobs() []Job {
	return []Job{
		{ID: 1, TaskID: 84, Name: "checks", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess, RawStatus: StatusSuccess},
		{ID: 2, TaskID: 86, Name: "tofu (tailscale)", Status: ci.StatusInProgress, RawStatus: StatusRunning},
	}
}

// TestListTasksSendsPageAndLimitTogether covers the verified gotcha: `limit`
// alone is ignored and the endpoint answers with every row it has. A test that
// only checked limit would pass against the broken form.
func TestListTasksSendsPageAndLimitTogether(t *testing.T) {
	var got url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write(fixture(t, "tasks_page.json"))
	})
	c := testClient(t, mux)

	if _, err := c.listTasks(context.Background(), "acme/app", 1); err != nil {
		t.Fatal(err)
	}
	if got.Get("page") == "" {
		t.Error("page must be sent; without it the endpoint ignores limit and returns every row")
	}
	if got.Get("limit") == "" {
		t.Error("limit must be sent")
	}
}

func TestStampLiveTimingsJoinsByTaskID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// Unfiltered on purpose: the finished shards of the run are what give a
		// group its first start, and what the run is measured by when it ends.
		if got := q["status"]; len(got) != 0 {
			t.Errorf("status filter = %v, want none", got)
		}
		if q.Get("page") != "1" || q.Get("limit") == "" {
			t.Errorf("query = %v, want page=1 with a limit", q)
		}
		_, _ = w.Write(fixture(t, "tasks_page.json"))
	})
	c := testClient(t, mux)

	jobs := c.stampLiveTimings(context.Background(), "acme/app", runningJobs())

	// task 86 is "tofu (tailscale)", still running: it gets a start and no end.
	if jobs[1].StartedAt.IsZero() {
		t.Fatal("the running job must be stamped with its start")
	}
	if !jobs[1].CompletedAt.IsZero() {
		t.Error("a running task has no completion to copy")
	}
	// task 84 is terminal in the fixture, so it gets both: a finished shard's
	// start is the group's start, and its completion is the run's measurement.
	if jobs[0].StartedAt.IsZero() || jobs[0].CompletedAt.IsZero() {
		t.Error("the terminal job should have been stamped too")
	}
	if got := jobs[0].Duration(); got != 4*time.Second {
		t.Errorf("checks duration = %v, want 4s from the live page", got)
	}
}

func TestStampLiveTimingsJoinMissLeavesZero(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, _ *http.Request) {
		// A page with none of the task ids we asked about.
		_, _ = w.Write([]byte(`{"total_count":1,"workflow_runs":[{"id":9999,"name":"other","status":"running","run_started_at":"2026-07-30T10:00:00Z"}]}`))
	})
	c := testClient(t, mux)

	jobs := c.stampLiveTimings(context.Background(), "acme/app", runningJobs())
	for i, j := range jobs {
		if !j.StartedAt.IsZero() || !j.CompletedAt.IsZero() {
			t.Errorf("job %d was stamped from a non-matching task", i)
		}
	}
	// The degrade must be total: no anchor rather than a wrong one, and it must
	// decline for the missing start rather than for some unrelated gate.
	_, _, ok, why := ci.LiveAnchor(ci.ComputeSteps(toCIJobsForTest(jobs)),
		map[string]float64{"tofu": 300}, time.Now(), time.Hour)
	if ok {
		t.Error("an unstamped step must not produce a live window")
	}
	if why != ci.DeclineNoStart {
		t.Errorf("declined for %q, want %q", why, ci.DeclineNoStart)
	}
}

func TestStampLiveTimingsErrorIsNotFatal(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	jobs := c.stampLiveTimings(context.Background(), "acme/app", runningJobs())
	if len(jobs) != 2 {
		t.Fatalf("jobs were dropped: %d", len(jobs))
	}
	if !jobs[1].StartedAt.IsZero() {
		t.Error("a failed lookup must leave the stamps zero")
	}
}

// TestStampLiveTimingsSkippedBetweenWaves: with nothing running and the next
// wave still waiting for a runner there is nothing on the page worth a request.
// Not started at all is the same case.
func TestStampLiveTimingsSkippedBetweenWaves(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write(fixture(t, "tasks_page.json"))
	}))
	for _, jobs := range [][]Job{
		{
			{ID: 1, TaskID: 84, Name: "checks", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess, RawStatus: StatusSuccess},
			{ID: 2, TaskID: 0, Name: "detect", Status: ci.StatusQueued, RawStatus: StatusWaiting},
		},
		{
			{ID: 1, TaskID: 0, Name: "checks", Status: ci.StatusQueued, RawStatus: StatusWaiting},
		},
	} {
		c.stampLiveTimings(context.Background(), "acme/app", jobs)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("made %d requests with nothing running and nothing finished to measure, want 0", n)
	}
}

// TestStampLiveTimingsMeasuresAFinishedRun is the final tick of a run: nothing
// is running any more, and the completed rows are exactly what the next run of
// this workflow will be seeded from.
func TestStampLiveTimingsMeasuresAFinishedRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", serveFixture(t, "tasks_page.json"))
	c := testClient(t, mux)

	jobs := c.stampLiveTimings(context.Background(), "acme/app", []Job{
		{ID: 1, TaskID: 84, Name: "checks", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess, RawStatus: StatusSuccess},
		{ID: 2, TaskID: 85, Name: "detect", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess, RawStatus: StatusSuccess},
	})
	weights := ci.GroupWeights(toCIJobsForTest(jobs))
	if weights["checks"] != 4 || weights["detect"] != 3 {
		t.Errorf("weights = %v, want checks=4 detect=3 measured live", weights)
	}
}

// taskRow builds a task row with real timestamps, the way the API sends them.
func taskRow(id int64, name, status string, started, updated time.Time) wireTask {
	t := wireTask{ID: id, Name: name, Status: status}
	t.RunStartedAt.set(started)
	t.UpdatedAt.set(updated)
	return t
}

// TestJoinTasksBoundsCompletionByTheRunStop is the production case: Forgejo
// touched the finished rows a day after the run, so updated_at read as a
// forty-hour job. With the run's own stop known, a completion past it is
// refused - the start stays, since it still says when the group began - while
// one inside the slack is the genuine article, and no stop bounds nothing.
func TestJoinTasksBoundsCompletionByTheRunStop(t *testing.T) {
	started := time.Date(2026, 9, 1, 4, 7, 0, 0, time.UTC)
	stopped := time.Date(2026, 9, 1, 4, 12, 36, 0, time.UTC)
	tests := []struct {
		name          string
		updated       time.Time
		stopped       time.Time
		wantRewritten int
		wantDuration  time.Duration
	}{
		{name: "rewritten two days later", updated: stopped.Add(42 * time.Hour), stopped: stopped, wantRewritten: 1},
		{name: "inside the slack", updated: stopped.Add(4 * time.Second), stopped: stopped, wantDuration: 5*time.Minute + 40*time.Second},
		{name: "before the stop", updated: stopped.Add(-10 * time.Second), stopped: stopped, wantDuration: 5*time.Minute + 26*time.Second},
		{name: "no stop to bound against", updated: stopped.Add(42 * time.Hour), wantDuration: 42*time.Hour + 5*time.Minute + 36*time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			jobs := []Job{{ID: 1, TaskID: 1, Name: "test", RawStatus: StatusSuccess}}
			tasks := []wireTask{taskRow(1, "test", StatusSuccess, started, tc.updated)}

			matched, rewritten := joinTasks(jobs, tasks, 62, tc.stopped)
			if matched != 1 || rewritten != tc.wantRewritten {
				t.Fatalf("matched=%d rewritten=%d, want 1 and %d", matched, rewritten, tc.wantRewritten)
			}
			if !jobs[0].StartedAt.Equal(started) {
				t.Errorf("StartedAt = %v, want the row's start kept", jobs[0].StartedAt)
			}
			if got := jobs[0].Duration(); got != tc.wantDuration {
				t.Errorf("duration = %v, want %v", got, tc.wantDuration)
			}
		})
	}

	// Mixed rows, as one runner lane's rows were rewritten and another's were
	// not: only the intact group is measured, the other floors and takes the mean.
	jobs := []Job{
		{ID: 1, TaskID: 1, Name: "analysis", RawStatus: StatusSuccess},
		{ID: 2, TaskID: 2, Name: "test", RawStatus: StatusSuccess},
	}
	tasks := []wireTask{
		taskRow(1, "analysis", StatusSuccess, started.Add(-time.Minute), stopped.Add(42*time.Hour)),
		taskRow(2, "test", StatusSuccess, started, stopped.Add(-10*time.Second)),
	}
	joinTasks(jobs, tasks, 62, stopped)
	weights := ci.GroupWeights(toCIJobsForTest(jobs))
	if weights["analysis"] != ci.StepWeightFloor || weights["test"] != 326 {
		t.Errorf("weights = %v, want analysis at the floor and test=326", weights)
	}
}

func TestStampHistoricTimingsStopsWhenAllMatched(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write(fixture(t, "tasks_page.json"))
	})
	c := testClient(t, mux)

	jobs := []Job{{ID: 1, TaskID: 84, Name: "checks", Status: ci.StatusCompleted, RawStatus: StatusSuccess}}
	c.stampHistoricTimings(context.Background(), "acme/app", jobs, 33, time.Time{})
	if n := calls.Load(); n != 1 {
		t.Errorf("made %d requests, want 1 once every task was matched", n)
	}
}

// TestStampHistoricTimingsPageCap bounds the walk when the rows never turn up.
func TestStampHistoricTimingsPageCap(t *testing.T) {
	var calls atomic.Int32
	full := noisePage()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(full))
	})
	c := testClient(t, mux)

	jobs := []Job{{ID: 1, TaskID: 84, Name: "checks", Status: ci.StatusCompleted, RawStatus: StatusSuccess}}
	jobs = c.stampHistoricTimings(context.Background(), "acme/app", jobs, 33, time.Time{})

	if n := int(calls.Load()); n != maxTaskPages {
		t.Errorf("made %d page requests, want the cap of %d", n, maxTaskPages)
	}
	if !jobs[0].StartedAt.IsZero() {
		t.Error("an unmatched job must stay unstamped")
	}
}

// TestStampHistoricTimingsReachesBuriedRows covers the run a page-1-only walk
// gives up on - a busy repo, or a workflow whose prior run other workflows have
// buried. Such a run comes back fully unmeasured, costing it both duration-sized
// pills and, since LiveAnchor then has nothing to estimate from, its live ETA.
func TestStampHistoricTimingsReachesBuriedRows(t *testing.T) {
	noise := noisePage()

	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// The row we want sits well past page 1, but inside the cap.
		if r.URL.Query().Get("page") == "4" {
			_, _ = w.Write(fixture(t, "tasks_page.json"))
			return
		}
		_, _ = w.Write([]byte(noise))
	})
	c := testClient(t, mux)

	jobs := []Job{{ID: 1, TaskID: 84, Name: "checks", Status: ci.StatusCompleted, RawStatus: StatusSuccess}}
	jobs = c.stampHistoricTimings(context.Background(), "acme/app", jobs, 33, time.Time{})

	if got := jobs[0].Duration(); got != 4*time.Second {
		t.Fatalf("checks duration = %v, want 4s from the page-4 row", got)
	}
	// It must also stop as soon as it has what it came for, not walk to the cap.
	if n := calls.Load(); n != 4 {
		t.Errorf("made %d page requests, want 4", n)
	}
}

func TestStampHistoricTimingsDisabledByOptions(t *testing.T) {
	var calls atomic.Int32
	srv := newStubServer(t, &calls, fixture(t, "tasks_page.json"))
	c := NewClient(srv, "t", Options{Timeout: time.Second}) // HistoryTimings false
	c.stampHistoricTimings(context.Background(), "acme/app", runningJobs(), 33, time.Time{})
	if n := calls.Load(); n != 0 {
		t.Errorf("made %d requests with history timings off, want 0", n)
	}
}

// TestJoinTasksRunNumberMismatchStillJoins documents that the id join wins: task
// ids are unique repo-wide, so a run_number that disagrees is worth a debug line
// and nothing more.
func TestJoinTasksRunNumberMismatchStillJoins(t *testing.T) {
	jobs := []Job{{ID: 1, TaskID: 84, Name: "checks", RawStatus: StatusSuccess}}
	tasks := []wireTask{{ID: 84, Name: "checks", Status: StatusSuccess, RunNumber: 999}}
	// A real timestamp so the join is observable.
	_ = tasks[0].RunStartedAt.UnmarshalJSON([]byte(`"2026-07-30T10:00:00Z"`))
	_ = tasks[0].UpdatedAt.UnmarshalJSON([]byte(`"2026-07-30T10:00:05Z"`))

	if matched, _ := joinTasks(jobs, tasks, 33, time.Time{}); matched != 1 {
		t.Fatalf("matched %d, want 1", matched)
	}
	if jobs[0].Duration() != 5*time.Second {
		t.Errorf("duration = %v, want 5s", jobs[0].Duration())
	}
}

func TestJoinTasksHandlesEmptyInputs(t *testing.T) {
	if n, _ := joinTasks(nil, []wireTask{{ID: 1}}, 0, time.Time{}); n != 0 {
		t.Errorf("joinTasks(nil jobs) = %d", n)
	}
	if n, _ := joinTasks([]Job{{TaskID: 1}}, nil, 0, time.Time{}); n != 0 {
		t.Errorf("joinTasks(nil tasks) = %d", n)
	}
	// A job with no task id can never join.
	if n, _ := joinTasks([]Job{{TaskID: 0}}, []wireTask{{ID: 0}}, 0, time.Time{}); n != 1 {
		t.Logf("task id 0 joined; harmless, but the caller does not count it as wanted")
	}
}

// noisePage is a full page of task rows matching none of the ids under test: it
// is what pushes a run's own rows onto a later page. Full-length on purpose, so
// the walk does not stop early on a short page.
func noisePage() string {
	rows := make([]string, taskPageSize)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"id":%d,"name":"other","status":"success"}`, 10000+i)
	}
	return `{"total_count":9999,"workflow_runs":[` + strings.Join(rows, ",") + `]}`
}

// newStubServer returns a server URL that counts every request.
func newStubServer(t *testing.T, calls *atomic.Int32, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
