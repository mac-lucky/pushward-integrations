package forgejo

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/ci"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	// #nosec G304 -- name is a test-local literal naming a checked-in fixture.
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func decodeRun(t *testing.T, name string) Run {
	t.Helper()
	var w wireRun
	if err := json.Unmarshal(fixture(t, name), &w); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return toRun(w)
}

// TestDecodeRunIndexInRepoIsNotID is the trap this bridge is most likely to fall
// into. Forgejo builds html_url from index_in_repo while the API is addressed by
// id, so a locally-composed URL points at a different run entirely.
func TestDecodeRunIndexInRepoIsNotID(t *testing.T) {
	run := decodeRun(t, "run_success.json")

	if run.ID == run.IndexInRepo {
		t.Fatal("the fixture must keep id and index_in_repo distinct or it proves nothing")
	}
	if run.ID != 39 || run.IndexInRepo != 33 {
		t.Fatalf("id/index = %d/%d, want 39/33", run.ID, run.IndexInRepo)
	}
	if !strings.HasSuffix(run.HTMLURL, "/actions/runs/"+strconv.FormatInt(run.IndexInRepo, 10)) {
		t.Errorf("html_url %q is not built from index_in_repo", run.HTMLURL)
	}
	if strings.HasSuffix(run.HTMLURL, "/actions/runs/"+strconv.FormatInt(run.ID, 10)) {
		t.Errorf("html_url %q looks like it was built from the API id", run.HTMLURL)
	}
}

// TestRunFixturesHaveNoConclusionKey lets the fixtures themselves certify that
// the field the github bridge relies on simply does not exist here.
func TestRunFixturesHaveNoConclusionKey(t *testing.T) {
	for _, name := range []string{"run_success.json", "run_failure.json", "run_dispatch_empty_event.json"} {
		if bytes.Contains(fixture(t, name), []byte(`"conclusion"`)) {
			t.Errorf("%s carries a conclusion key; a Forgejo run has none", name)
		}
	}
}

func TestDecodeRunSuccess(t *testing.T) {
	run := decodeRun(t, "run_success.json")

	if run.Status != ci.StatusCompleted || run.Conclusion != ci.ConclusionSuccess {
		t.Errorf("status/conclusion = %q/%q", run.Status, run.Conclusion)
	}
	if !run.Terminal() {
		t.Error("a successful run must be terminal")
	}
	if run.WorkflowID != "tofu.yml" {
		t.Errorf("workflow_id = %q; it is a filename string, not an int", run.WorkflowID)
	}
	if run.Name != "tofu" {
		t.Errorf("display name = %q, want the filename without its extension", run.Name)
	}
	if run.HeadBranch != "master" {
		t.Errorf("head branch = %q, want the bare prettyref", run.HeadBranch)
	}
	if run.Duration != 22*time.Second {
		t.Errorf("duration = %v, want 22s (the wire value is nanoseconds)", run.Duration)
	}
	if run.RepoFullName != "acme/app" {
		t.Errorf("repo = %q", run.RepoFullName)
	}
	if run.StartedAt.IsZero() || run.StoppedAt.IsZero() {
		t.Error("a terminal run must carry both a start and a stop")
	}
}

func TestDecodeRunFailure(t *testing.T) {
	run := decodeRun(t, "run_failure.json")
	if run.Status != ci.StatusCompleted || run.Conclusion != ci.ConclusionFailure {
		t.Errorf("status/conclusion = %q/%q", run.Status, run.Conclusion)
	}
	if !ci.JobFailed(run.Conclusion) {
		t.Error("a failed run must read as failed to the shared ladder")
	}
}

// TestDecodeRunEmptyEvent covers the run whose `event` is blank while
// `trigger_event` carries the real value.
func TestDecodeRunEmptyEvent(t *testing.T) {
	run := decodeRun(t, "run_dispatch_empty_event.json")
	if run.Event != "workflow_dispatch" {
		t.Errorf("event = %q, want the trigger_event fallback", run.Event)
	}
}

func TestDecodeActiveEmpty(t *testing.T) {
	var resp wireRunsResponse
	if err := json.Unmarshal(fixture(t, "runs_active_empty.json"), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TotalCount != 0 || len(resp.WorkflowRuns) != 0 {
		t.Errorf("idle probe decoded to %d runs", len(resp.WorkflowRuns))
	}
}

// TestDecodeJobsIsABareArray pins that the jobs endpoint has no envelope. If it
// ever grows one, decoding into a slice fails loudly here rather than silently
// producing zero jobs in production.
func TestDecodeJobsIsABareArray(t *testing.T) {
	raw := fixture(t, "jobs_matrix.json")

	var envelope wireRunsResponse
	if err := json.Unmarshal(raw, &envelope); err == nil {
		t.Error("the fixture decoded as an envelope; it must be a bare array")
	}

	var wires []wireJob
	if err := json.Unmarshal(raw, &wires); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	if len(wires) != 4 {
		t.Fatalf("got %d jobs, want 4", len(wires))
	}

	jobs := make([]Job, 0, len(wires))
	for _, w := range wires {
		jobs = append(jobs, toJob(w))
	}

	if jobs[0].Needs != nil {
		t.Errorf("a null needs must decode to nil, got %v", jobs[0].Needs)
	}
	if len(jobs[2].Needs) != 2 {
		t.Errorf("needs = %v, want two entries", jobs[2].Needs)
	}
	if jobs[2].TaskID != 86 {
		t.Errorf("task_id = %d, want 86 (the tasks join key)", jobs[2].TaskID)
	}
	for i, j := range jobs {
		if !j.StartedAt.IsZero() || !j.CompletedAt.IsZero() {
			t.Errorf("job %d carries timestamps; the jobs endpoint has none to give", i)
		}
	}
	if jobs[2].Status != ci.StatusInProgress {
		t.Errorf("running job status = %q", jobs[2].Status)
	}
	if jobs[3].Status != ci.StatusQueued {
		t.Errorf("waiting job status = %q, want queued", jobs[3].Status)
	}

	// The three tofu legs fold into one group; checks and detect stay separate.
	info := ci.ComputeSteps(toCIJobsForTest(jobs))
	if info.TotalSteps != 3 {
		t.Errorf("TotalSteps = %d, want 3 (checks, detect, tofu)", info.TotalSteps)
	}
	if info.CurrentStepName != "tofu" {
		t.Errorf("CurrentStepName = %q, want the folded matrix group", info.CurrentStepName)
	}
}

// TestDecodeTasksEnvelopeHoldsJobs pins the endpoint's misleading shape: the JSON
// key is "workflow_runs" but every row is one job.
func TestDecodeTasksEnvelopeHoldsJobs(t *testing.T) {
	var resp wireTasksResponse
	if err := json.Unmarshal(fixture(t, "tasks_page.json"), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Tasks) != 4 {
		t.Fatalf("got %d tasks, want 4", len(resp.Tasks))
	}
	// Every row belongs to the same run, which is what makes these per-job.
	for _, task := range resp.Tasks {
		if task.RunNumber != 33 {
			t.Errorf("task %d has run_number %d, want 33", task.ID, task.RunNumber)
		}
	}
	names := map[string]bool{}
	for _, task := range resp.Tasks {
		names[task.Name] = true
	}
	if !names["tofu (tailscale)"] {
		t.Error("task names must carry the job name including matrix parens")
	}
	// The unstarted task's epoch stamps must read as absent.
	for _, task := range resp.Tasks {
		if task.Status == StatusWaiting && !task.RunStartedAt.IsZero() {
			t.Errorf("waiting task %d has a start of %v; the epoch means unset",
				task.ID, task.RunStartedAt.Time())
		}
	}
}

func TestWorkflowDisplayName(t *testing.T) {
	tests := []struct {
		workflowID string
		title      string
		want       string
	}{
		{"tofu.yml", "some commit", "tofu"},
		{"ci-cd.yaml", "some commit", "ci-cd"},
		{".forgejo/workflows/release.yml", "some commit", "release"},
		{"noext", "some commit", "noext"},
		{"", "Add the widget", "Add the widget"},
		{"", "", "workflow"},
		{"   ", "   ", "workflow"},
	}
	for _, tc := range tests {
		if got := workflowDisplayName(tc.workflowID, tc.title); got != tc.want {
			t.Errorf("workflowDisplayName(%q, %q) = %q, want %q", tc.workflowID, tc.title, got, tc.want)
		}
	}
}

// TestRunTitleIsTruncated keeps an unbounded commit subject out of memory and
// out of anything that might render it.
func TestRunTitleIsTruncated(t *testing.T) {
	var w wireRun
	w.Title = strings.Repeat("x", 5000)
	w.Status = StatusSuccess
	run := toRun(w)
	if len([]rune(run.Title)) > maxTitleLen {
		t.Errorf("title kept %d runes, want at most %d", len([]rune(run.Title)), maxTitleLen)
	}
}

// toCIJobsForTest mirrors the poller's converter so this package can exercise
// the ladder against real fixtures without importing it.
func toCIJobsForTest(jobs []Job) []ci.Job {
	out := make([]ci.Job, len(jobs))
	for i, j := range jobs {
		out[i] = ci.Job{
			Name:        j.Name,
			Status:      j.Status,
			Conclusion:  j.Conclusion,
			StartedAt:   j.StartedAt,
			CompletedAt: j.CompletedAt,
		}
	}
	return out
}
