package gitea

import "testing"

func TestBaseJobName(t *testing.T) {
	cases := map[string]string{
		"ci-cd / Build (ubuntu, node-16)": "Build",
		"ci-cd / Setup Build Environment": "Setup Build Environment",
		"Build (ubuntu)":                  "Build",
		"Test":                            "Test",
	}
	for in, want := range cases {
		if got := baseJobName(in); got != want {
			t.Errorf("baseJobName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestComputeStepsMatrixGrouping(t *testing.T) {
	jobs := []jobRecord{
		{ID: 1, Name: "Build (ubuntu)", Status: "completed", Conclusion: "success"},
		{ID: 2, Name: "Build (windows)", Status: "in_progress"},
		{ID: 3, Name: "Test", Status: "queued"},
	}
	info := computeSteps(jobs)

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

func TestComputeStepsAllCompletedWithFailure(t *testing.T) {
	jobs := []jobRecord{
		{ID: 1, Name: "Build", Status: "completed", Conclusion: "success"},
		{ID: 2, Name: "Test", Status: "completed", Conclusion: "failure"},
	}
	info := computeSteps(jobs)
	if !info.AllCompleted {
		t.Error("expected AllCompleted true")
	}
	if !info.AnyFailed {
		t.Error("expected AnyFailed true")
	}
	if info.Progress != 1.0 {
		t.Errorf("expected progress 1.0, got %v", info.Progress)
	}
}

func TestStepRowsLabelsCapOmitsAboveTen(t *testing.T) {
	info := stepInfo{TotalSteps: 11, StepRows: make([]int, 11), StepLabels: make([]string, 11)}
	rows, labels := stepRowsLabels(info)
	if rows != nil || labels != nil {
		t.Fatalf("expected nil rows/labels above the 10-group cap, got rows=%v labels=%v", rows, labels)
	}
}

func TestStepRowsLabelsClampsAndTruncates(t *testing.T) {
	rows := []int{15, 0, 3}
	labels := []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "short", "mid"} // first is 42 chars
	info := stepInfo{TotalSteps: 3, StepRows: rows, StepLabels: labels}

	gotRows, gotLabels := stepRowsLabels(info)
	if len(gotRows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(gotRows))
	}
	if gotRows[0] != 10 {
		t.Errorf("expected row count clamped to 10, got %d", gotRows[0])
	}
	if gotRows[1] != 1 {
		t.Errorf("expected row count clamped up to 1, got %d", gotRows[1])
	}
	if len([]rune(gotLabels[0])) != 32 {
		t.Errorf("expected label truncated to 32 runes, got %d", len([]rune(gotLabels[0])))
	}
}

func TestConclusionState(t *testing.T) {
	cases := []struct {
		conclusion string
		wantState  string
	}{
		{"success", "Success"},
		{"failure", "Failed"},
		{"cancelled", "Cancelled"},
		{"skipped", "Skipped"},
		{"", "Complete"},
	}
	for _, c := range cases {
		got, _ := conclusionState(c.conclusion)
		if got != c.wantState {
			t.Errorf("conclusionState(%q) state = %q, want %q", c.conclusion, got, c.wantState)
		}
	}
}
