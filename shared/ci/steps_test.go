package ci

import (
	"testing"
	"time"
)

func TestBaseJobName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Build (ubuntu, node-16)", "Build"},
		{"Test", "Test"},
		{"ci-cd / Code Analysis (go-vet)", "Code Analysis"},
		{"ci-cd / Setup Build Environment", "Setup Build Environment"},
		{"ci-cd / Go Tests", "Go Tests"},
		{"ci-cd / Build (ubuntu, node-16)", "Build"},
		{"Build (ubuntu)", "Build"},
		{"Deploy (prod)", "Deploy"},
		{"NoParens", "NoParens"},
		{"Has (Parens) Mid", "Has (Parens) Mid"}, // no trailing )
	}
	for _, tt := range tests {
		got := BaseJobName(tt.input)
		if got != tt.want {
			t.Errorf("BaseJobName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestJobFailed(t *testing.T) {
	cases := map[string]bool{
		ConclusionFailure:        true,
		ConclusionCancelled:      true,
		ConclusionTimedOut:       true,
		ConclusionStartupFailure: true,
		ConclusionSuccess:        false,
		// Skipped is a path not taken, not a failure. Forgejo relies on this:
		// an if-gated job it reports as "skipped" must not redden the run.
		ConclusionSkipped: false,
		"":                false,
		"neutral":         false,
	}
	for conclusion, want := range cases {
		if got := JobFailed(conclusion); got != want {
			t.Errorf("JobFailed(%q) = %v, want %v", conclusion, got, want)
		}
	}
}

func TestComputeSteps_Empty(t *testing.T) {
	info := ComputeSteps(nil)
	if info.TotalSteps != 0 {
		t.Errorf("expected TotalSteps=0, got %d", info.TotalSteps)
	}
	if info.Progress != 0 {
		t.Errorf("expected Progress=0, got %f", info.Progress)
	}
	if !info.AllCompleted {
		// All zero jobs means all completed
		t.Error("expected AllCompleted=true for empty jobs")
	}
}

func TestComputeSteps_AllQueued(t *testing.T) {
	jobs := []Job{
		{Name: "Lint", Status: StatusQueued},
		{Name: "Build", Status: StatusQueued},
		{Name: "Test", Status: StatusQueued},
	}
	info := ComputeSteps(jobs)
	if info.TotalSteps != 3 {
		t.Errorf("expected TotalSteps=3, got %d", info.TotalSteps)
	}
	if info.CurrentStepName != "Queued" {
		t.Errorf("expected CurrentStepName=Queued, got %s", info.CurrentStepName)
	}
	if info.CurrentStep != 1 {
		t.Errorf("expected CurrentStep=1, got %d", info.CurrentStep)
	}
	if info.AllCompleted {
		t.Error("expected AllCompleted=false")
	}
	if info.Progress != 0.0 {
		t.Errorf("expected Progress=0.0, got %f", info.Progress)
	}
	if len(info.StepRows) != 3 || info.StepRows[0] != 1 || info.StepRows[1] != 1 || info.StepRows[2] != 1 {
		t.Errorf("expected StepRows=[1,1,1], got %v", info.StepRows)
	}
}

// TestComputeSteps_UnknownStatusIsPending pins that a status this package does
// not recognise counts as not-yet-running rather than done. Forgejo has three
// such values ("waiting", "blocked", "unknown"), and treating any of them as
// completed would end a Live Activity while its run was still going.
func TestComputeSteps_UnknownStatusIsPending(t *testing.T) {
	for _, status := range []string{"waiting", "blocked", "unknown", ""} {
		info := ComputeSteps([]Job{
			{Name: "Build", Status: StatusCompleted, Conclusion: ConclusionSuccess},
			{Name: "Deploy", Status: status},
		})
		if info.AllCompleted {
			t.Errorf("status %q: expected AllCompleted=false", status)
		}
		if info.CurrentStepName != "Queued" || info.CurrentStep != 2 {
			t.Errorf("status %q: expected the queued fallback on step 2, got %q (step %d)",
				status, info.CurrentStepName, info.CurrentStep)
		}
		if info.Progress != 0.5 {
			t.Errorf("status %q: expected Progress=0.5, got %f", status, info.Progress)
		}
	}
}

func TestStepColor(t *testing.T) {
	cases := map[string]string{
		"Test":           "yellow",
		"unit-tests":     "yellow",
		"pytest":         "yellow",
		"Lint":           "purple",
		"golangci-lint":  "purple",
		"Build":          "blue",
		"Build (ubuntu)": "blue",
		"Docker Build":   "blue", // build-family keyword wins over docker by switch order
		"Push image":     "cyan",
		"Deploy":         "green",
		"release":        "green",
		"CodeQL":         "orange",
		"security-scan":  "orange",
		"Something Else": "", // unmatched falls back to the accent color
	}
	for name, want := range cases {
		if got := StepColor(name); got != want {
			t.Errorf("StepColor(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestComputeSteps_StepColors(t *testing.T) {
	jobs := []Job{
		{Name: "Test", Status: StatusInProgress},
		{Name: "Build", Status: StatusQueued},
		{Name: "Deploy", Status: StatusQueued},
	}
	info := ComputeSteps(jobs)
	// step_colors must be one-per-step so the server's length check passes.
	if len(info.StepColors) != info.TotalSteps {
		t.Fatalf("expected StepColors length %d, got %d (%v)", info.TotalSteps, len(info.StepColors), info.StepColors)
	}
	want := []string{"yellow", "blue", "green"}
	for i, w := range want {
		if info.StepColors[i] != w {
			t.Errorf("StepColors[%d] = %q, want %q", i, info.StepColors[i], w)
		}
	}
}

func TestComputeSteps_MatrixJobs(t *testing.T) {
	jobs := []Job{
		{Name: "Build (ubuntu, node-16)", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "Build (ubuntu, node-18)", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "Build (ubuntu, node-20)", Status: StatusInProgress},
		{Name: "Test", Status: StatusQueued},
	}
	info := ComputeSteps(jobs)
	if info.TotalSteps != 2 {
		t.Errorf("expected TotalSteps=2, got %d", info.TotalSteps)
	}
	if info.CurrentStepName != "Build" {
		t.Errorf("expected CurrentStepName=Build, got %s", info.CurrentStepName)
	}
	if info.CurrentStep != 1 {
		t.Errorf("expected CurrentStep=1, got %d", info.CurrentStep)
	}
	if len(info.StepRows) != 2 || info.StepRows[0] != 3 || info.StepRows[1] != 1 {
		t.Errorf("expected StepRows=[3,1], got %v", info.StepRows)
	}
	if info.Progress != 0.5 {
		t.Errorf("expected Progress=0.5, got %f", info.Progress)
	}
}

// TestComputeSteps_MatrixGroupingLabels carries the relay's variant of the
// matrix case: it pins the label list and the exact 1/3 progress fraction, which
// the node-matrix case above does not.
func TestComputeSteps_MatrixGroupingLabels(t *testing.T) {
	jobs := []Job{
		{Name: "Build (ubuntu)", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "Build (windows)", Status: StatusInProgress},
		{Name: "Test", Status: StatusQueued},
	}
	info := ComputeSteps(jobs)

	if info.TotalSteps != 2 {
		t.Fatalf("expected 2 step groups (Build, Test), got %d", info.TotalSteps)
	}
	if info.StepRows[0] != 2 {
		t.Errorf("expected Build group to hold 2 jobs, got %d", info.StepRows[0])
	}
	if info.StepLabels[0] != "Build" || info.StepLabels[1] != "Test" {
		t.Errorf("unexpected labels: %v", info.StepLabels)
	}
	if info.CurrentStepName != "Build" || info.CurrentStep != 1 {
		t.Errorf("expected active group Build (step 1), got %q (step %d)", info.CurrentStepName, info.CurrentStep)
	}
	if info.AllCompleted {
		t.Error("expected AllCompleted false")
	}
	if info.Progress != float64(1)/float64(3) {
		t.Errorf("expected progress 1/3, got %v", info.Progress)
	}
}

func TestComputeSteps_AllCompletedSuccess(t *testing.T) {
	jobs := []Job{
		{Name: "Lint", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "Build", Status: StatusCompleted, Conclusion: ConclusionSuccess},
	}
	info := ComputeSteps(jobs)
	if !info.AllCompleted {
		t.Error("expected AllCompleted=true")
	}
	if info.AnyFailed {
		t.Error("expected AnyFailed=false")
	}
	if info.TotalSteps != 2 {
		t.Errorf("expected TotalSteps=2, got %d", info.TotalSteps)
	}
	if info.Progress != 1.0 {
		t.Errorf("expected Progress=1.0, got %f", info.Progress)
	}
}

func TestComputeSteps_WithFailure(t *testing.T) {
	jobs := []Job{
		{Name: "Lint", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "Build", Status: StatusCompleted, Conclusion: ConclusionFailure},
	}
	info := ComputeSteps(jobs)
	if !info.AllCompleted {
		t.Error("expected AllCompleted=true")
	}
	if !info.AnyFailed {
		t.Error("expected AnyFailed=true")
	}
	// Carried from the relay's copy: a failed run still reads as fully done.
	if info.Progress != 1.0 {
		t.Errorf("expected progress 1.0, got %v", info.Progress)
	}
}

func TestComputeSteps_WithCancelled(t *testing.T) {
	jobs := []Job{
		{Name: "Lint", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "Build", Status: StatusCompleted, Conclusion: ConclusionCancelled},
	}
	info := ComputeSteps(jobs)
	if !info.AnyFailed {
		t.Error("expected AnyFailed=true for cancelled job")
	}
}

func TestComputeSteps_ReusableWorkflowMatrix(t *testing.T) {
	// Simulates a real reusable workflow where jobs appear with "ci-cd / " prefix
	// and matrix parameters. All jobs visible from the start.
	jobs := []Job{
		{Name: "ci-cd / Check Code Changes", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "ci-cd / Setup Build Environment", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "ci-cd / Code Analysis (go-vet)", Status: StatusInProgress},
		{Name: "ci-cd / Code Analysis (staticcheck)", Status: StatusQueued},
		{Name: "ci-cd / Code Analysis (grype)", Status: StatusQueued},
		{Name: "ci-cd / Go Tests", Status: StatusInProgress},
		{Name: "ci-cd / Build Container Image", Status: StatusQueued},
		{Name: "ci-cd / Container Integration Test", Status: StatusCompleted, Conclusion: ConclusionSkipped},
		{Name: "ci-cd / Kubernetes Integration Test", Status: StatusCompleted, Conclusion: ConclusionSkipped},
		{Name: "ci-cd / Build (linux/amd64)", Status: StatusQueued},
		{Name: "ci-cd / Build (linux/arm64)", Status: StatusQueued},
		{Name: "ci-cd / Create Multi-arch Manifest", Status: StatusQueued},
		{Name: "ci-cd / Post-deployment Verification", Status: StatusQueued},
	}
	info := ComputeSteps(jobs)

	// 10 unique steps after matrix grouping:
	// Check Code Changes, Setup Build Environment, Code Analysis (x3),
	// Go Tests, Build Container Image, Container Integration Test,
	// Kubernetes Integration Test, Build (x2), Create Multi-arch Manifest,
	// Post-deployment Verification
	if info.TotalSteps != 10 {
		t.Errorf("expected TotalSteps=10, got %d", info.TotalSteps)
	}
	if info.CurrentStepName != "Code Analysis" {
		t.Errorf("expected CurrentStepName='Code Analysis', got %q", info.CurrentStepName)
	}
	if info.CurrentStep != 3 {
		t.Errorf("expected CurrentStep=3, got %d", info.CurrentStep)
	}
	// StepRows: [1,1,3,1,1,1,1,2,1,1]
	wantRows := []int{1, 1, 3, 1, 1, 1, 1, 2, 1, 1}
	if len(info.StepRows) != len(wantRows) {
		t.Fatalf("expected StepRows len=%d, got %d: %v", len(wantRows), len(info.StepRows), info.StepRows)
	}
	for i, v := range wantRows {
		if info.StepRows[i] != v {
			t.Errorf("StepRows[%d]: expected %d, got %d (full: %v)", i, v, info.StepRows[i], info.StepRows)
		}
	}
	// Progress: 4 completed (check, setup, container-test, k8s-test) out of 13 jobs
	expectedProgress := 4.0 / 13.0
	if info.Progress != expectedProgress {
		t.Errorf("expected Progress=%f, got %f", expectedProgress, info.Progress)
	}
}

func TestComputeSteps_LazyJobCreation(t *testing.T) {
	// Simulates the progressive job discovery issue: initially only 7 jobs
	// visible (5 steps), then more appear.

	// Poll 1: Only first few jobs exist
	jobsPoll1 := []Job{
		{Name: "ci-cd / Check Code Changes", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "ci-cd / Setup Build Environment", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "ci-cd / Code Analysis (go-vet)", Status: StatusInProgress},
		{Name: "ci-cd / Code Analysis (staticcheck)", Status: StatusQueued},
		{Name: "ci-cd / Code Analysis (grype)", Status: StatusQueued},
		{Name: "ci-cd / Go Tests", Status: StatusInProgress},
		{Name: "ci-cd / Build Container Image", Status: StatusQueued},
	}
	info1 := ComputeSteps(jobsPoll1)
	if info1.TotalSteps != 5 {
		t.Errorf("poll 1: expected TotalSteps=5, got %d", info1.TotalSteps)
	}

	// Poll 2: More jobs appeared (after code-analysis completed)
	jobsPoll2 := []Job{
		{Name: "ci-cd / Check Code Changes", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "ci-cd / Setup Build Environment", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "ci-cd / Code Analysis (go-vet)", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "ci-cd / Code Analysis (staticcheck)", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "ci-cd / Code Analysis (grype)", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "ci-cd / Go Tests", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "ci-cd / Build Container Image", Status: StatusInProgress},
		{Name: "ci-cd / Container Integration Test", Status: StatusCompleted, Conclusion: ConclusionSkipped},
		{Name: "ci-cd / Kubernetes Integration Test", Status: StatusCompleted, Conclusion: ConclusionSkipped},
		{Name: "ci-cd / Build (linux/amd64)", Status: StatusQueued},
		{Name: "ci-cd / Build (linux/arm64)", Status: StatusQueued},
		{Name: "ci-cd / Create Multi-arch Manifest", Status: StatusQueued},
		{Name: "ci-cd / Post-deployment Verification", Status: StatusQueued},
	}
	info2 := ComputeSteps(jobsPoll2)
	if info2.TotalSteps != 10 {
		t.Errorf("poll 2: expected TotalSteps=10, got %d", info2.TotalSteps)
	}

	// Verify that max clamping would work: total should go from 5 to 10
	if info2.TotalSteps <= info1.TotalSteps {
		t.Errorf("expected poll 2 total (%d) > poll 1 total (%d)", info2.TotalSteps, info1.TotalSteps)
	}
}

func TestComputeSteps_CurrentStepStartedAt(t *testing.T) {
	first := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	second := time.Date(2026, 1, 1, 0, 0, 25, 0, time.UTC)

	// A fan-out group's shards share one step deadline, so the step started when
	// its first shard did.
	info := ComputeSteps([]Job{
		{Name: "Lint", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "Build (macos)", Status: StatusInProgress, StartedAt: second},
		{Name: "Build (ubuntu)", Status: StatusInProgress, StartedAt: first},
	})
	if !info.CurrentStepStartedAt.Equal(first) {
		t.Errorf("CurrentStepStartedAt = %v, want the earliest shard start %v", info.CurrentStepStartedAt, first)
	}

	// An unstamped start must not fabricate an anchor. Every forge produces this:
	// GitHub before it stamps the job, Forgejo whenever the task join misses, the
	// relay always.
	info = ComputeSteps([]Job{{Name: "Build", Status: StatusInProgress}})
	if !info.CurrentStepStartedAt.IsZero() {
		t.Errorf("CurrentStepStartedAt = %v, want zero for an unstamped start", info.CurrentStepStartedAt)
	}

	// Nothing running: the queued fallback picks a step but not a start.
	info = ComputeSteps([]Job{{Name: "Build", Status: StatusQueued}})
	if !info.CurrentStepStartedAt.IsZero() {
		t.Errorf("CurrentStepStartedAt = %v, want zero while queued", info.CurrentStepStartedAt)
	}
}

func TestRealignStep(t *testing.T) {
	seeded := []string{"Lint", "Deploy", "Build", "Test"}

	tests := []struct {
		name    string
		current int
		from    []string
		to      []string
		want    int
	}{
		{
			// The common case: the forge reveals groups in the seeded order, so
			// the index already addresses the right label and must not move.
			name:    "live order is a prefix of the seeded list",
			current: 2, from: []string{"Lint", "Deploy"}, to: seeded, want: 2,
		},
		{
			// The bug: this run skipped the if-gated Deploy, so Build is second
			// live but third in the seeded list.
			name:    "a skipped group shifts the index",
			current: 2, from: []string{"Lint", "Build"}, to: seeded, want: 3,
		},
		{
			name: "trailing group", current: 2,
			from: []string{"Lint", "Test"}, to: seeded, want: 4,
		},
		{
			// A group the prior run never had: the seeded labels cannot describe
			// it, so leave the index alone rather than inventing a position.
			name: "group missing from the seeded list", current: 2,
			from: []string{"Lint", "Fuzz"}, to: seeded, want: 2,
		},
		{
			// A tracked run seeded without labels still reaches the clamp.
			name: "no seeded labels", current: 2,
			from: []string{"Lint", "Build"}, to: nil, want: 2,
		},
		{name: "nothing running", current: 0, from: []string{"Lint"}, to: seeded, want: 0},
		{
			name: "index past the live list", current: 3,
			from: []string{"Lint"}, to: seeded, want: 3,
		},
		{name: "no live labels", current: 1, from: nil, to: seeded, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RealignStep(tc.current, tc.from, tc.to); got != tc.want {
				t.Errorf("RealignStep(%d, %v, %v) = %d, want %d",
					tc.current, tc.from, tc.to, got, tc.want)
			}
		})
	}
}
