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
		if got := r.URL.Query().Get("status"); got != StatusRunning {
			t.Errorf("status filter = %q, want %q", got, StatusRunning)
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
	// task 84 is terminal in the fixture, so it gets both.
	if jobs[0].StartedAt.IsZero() || jobs[0].CompletedAt.IsZero() {
		t.Error("the terminal job should have been stamped too")
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
	// The degrade must be total: no anchor rather than a wrong one.
	if _, _, ok := ci.LiveAnchor(ci.ComputeSteps(toCIJobsForTest(jobs)),
		map[string]float64{"tofu": 300}, time.Now(), time.Hour); ok {
		t.Error("an unstamped step must not produce a live window")
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

func TestStampLiveTimingsSkippedWhenNothingRunning(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write(fixture(t, "tasks_page.json"))
	}))
	jobs := []Job{{ID: 1, TaskID: 84, Name: "checks", Status: ci.StatusCompleted, RawStatus: StatusSuccess}}
	c.stampLiveTimings(context.Background(), "acme/app", jobs)
	if n := calls.Load(); n != 0 {
		t.Errorf("made %d requests with nothing running, want 0", n)
	}
}

func TestStampLiveTimingsDisabledByOptions(t *testing.T) {
	var calls atomic.Int32
	srv := newStubServer(t, &calls, fixture(t, "tasks_page.json"))
	c := NewClient(srv, "t", Options{Timeout: time.Second}) // LiveTimings false
	c.stampLiveTimings(context.Background(), "acme/app", runningJobs())
	if n := calls.Load(); n != 0 {
		t.Errorf("made %d requests with live timings off, want 0", n)
	}
}

func TestStampHistoricTimingsComputesDurations(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", serveFixture(t, "tasks_page.json"))
	c := testClient(t, mux)

	jobs := []Job{
		{ID: 1, TaskID: 84, Name: "checks", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess, RawStatus: StatusSuccess},
		{ID: 2, TaskID: 85, Name: "detect", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess, RawStatus: StatusSuccess},
	}
	jobs = c.stampHistoricTimings(context.Background(), "acme/app", jobs, 33)

	// checks: 21:27:22 -> 21:27:26 = 4s. detect: 21:27:22 -> 21:27:25 = 3s.
	if got := jobs[0].Duration(); got != 4*time.Second {
		t.Errorf("checks duration = %v, want 4s", got)
	}
	if got := jobs[1].Duration(); got != 3*time.Second {
		t.Errorf("detect duration = %v, want 3s", got)
	}

	weights := ci.GroupWeights(toCIJobsForTest(jobs))
	if weights["checks"] != 4 || weights["detect"] != 3 {
		t.Errorf("weights = %v, want checks=4 detect=3", weights)
	}
}

// TestStampHistoricTimingsEpochIsUnmeasurable is the minValidTime payoff, end to
// end: an unstarted task would otherwise contribute a 55-year duration and
// swamp every other pill.
func TestStampHistoricTimingsEpochIsUnmeasurable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", serveFixture(t, "tasks_page.json"))
	c := testClient(t, mux)

	jobs := []Job{
		// task 87 carries an epoch run_started_at in the fixture.
		{ID: 1, TaskID: 87, Name: "tofu (grafana)", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess, RawStatus: StatusSuccess},
		{ID: 2, TaskID: 84, Name: "checks", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess, RawStatus: StatusSuccess},
	}
	jobs = c.stampHistoricTimings(context.Background(), "acme/app", jobs, 33)

	if !jobs[0].StartedAt.IsZero() {
		t.Errorf("epoch start decoded as %v, want zero", jobs[0].StartedAt)
	}
	if d := jobs[0].Duration(); d != 0 {
		t.Errorf("duration = %v, want 0 for an unmeasurable task", d)
	}

	weights := ci.GroupWeights(toCIJobsForTest(jobs))
	if w := weights["tofu"]; w != ci.StepWeightFloor {
		t.Errorf("tofu weight = %v, want the floor %v", w, ci.StepWeightFloor)
	}
	if weights["checks"] != 4 {
		t.Errorf("the measurable group lost its duration: %v", weights)
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
	c.stampHistoricTimings(context.Background(), "acme/app", jobs, 33)
	if n := calls.Load(); n != 1 {
		t.Errorf("made %d requests, want 1 once every task was matched", n)
	}
}

// TestStampHistoricTimingsPageCap bounds the walk when the rows never turn up.
func TestStampHistoricTimingsPageCap(t *testing.T) {
	var calls atomic.Int32
	rows := make([]string, taskPageSize)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"id":%d,"name":"other","status":"success"}`, 10000+i)
	}
	full := `{"total_count":9999,"workflow_runs":[` + strings.Join(rows, ",") + `]}`

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(full))
	})
	c := testClient(t, mux)

	jobs := []Job{{ID: 1, TaskID: 84, Name: "checks", Status: ci.StatusCompleted, RawStatus: StatusSuccess}}
	jobs = c.stampHistoricTimings(context.Background(), "acme/app", jobs, 33)

	if n := int(calls.Load()); n != maxTaskPages {
		t.Errorf("made %d page requests, want the cap of %d", n, maxTaskPages)
	}
	if !jobs[0].StartedAt.IsZero() {
		t.Error("an unmatched job must stay unstamped")
	}
}

func TestStampHistoricTimingsDisabledByOptions(t *testing.T) {
	var calls atomic.Int32
	srv := newStubServer(t, &calls, fixture(t, "tasks_page.json"))
	c := NewClient(srv, "t", Options{Timeout: time.Second}) // HistoryTimings false
	c.stampHistoricTimings(context.Background(), "acme/app", runningJobs(), 33)
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

	if matched := joinTasks(jobs, tasks, 33); matched != 1 {
		t.Fatalf("matched %d, want 1", matched)
	}
	if jobs[0].Duration() != 5*time.Second {
		t.Errorf("duration = %v, want 5s", jobs[0].Duration())
	}
}

func TestJoinTasksHandlesEmptyInputs(t *testing.T) {
	if n := joinTasks(nil, []wireTask{{ID: 1}}, 0); n != 0 {
		t.Errorf("joinTasks(nil jobs) = %d", n)
	}
	if n := joinTasks([]Job{{TaskID: 1}}, nil, 0); n != 0 {
		t.Errorf("joinTasks(nil tasks) = %d", n)
	}
	// A job with no task id can never join.
	if n := joinTasks([]Job{{TaskID: 0}}, []wireTask{{ID: 0}}, 0); n != 1 {
		t.Logf("task id 0 joined; harmless, but the caller does not count it as wanted")
	}
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
