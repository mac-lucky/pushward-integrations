package gitea

import (
	"testing"

	"github.com/mac-lucky/pushward-integrations/shared/ci"
)

// TestToCIJobs pins that the conversion passes the webhook vocabulary through
// untranslated and leaves both timestamps zero. Zero timestamps are what keep
// this provider on the static bar: with nothing measurable, ci.GroupWeights
// returns nil and ci.LiveAnchor declines.
func TestToCIJobs(t *testing.T) {
	got := toCIJobs([]jobRecord{
		{ID: 1, Name: "Build (ubuntu)", Status: "completed", Conclusion: "success"},
		{ID: 2, Name: "Test", Status: "in_progress"},
	})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Name != "Build (ubuntu)" || got[0].Status != ci.StatusCompleted || got[0].Conclusion != ci.ConclusionSuccess {
		t.Errorf("job 0 = %+v", got[0])
	}
	if got[1].Status != ci.StatusInProgress {
		t.Errorf("job 1 status = %q, want %q", got[1].Status, ci.StatusInProgress)
	}
	for i, j := range got {
		if !j.StartedAt.IsZero() || !j.CompletedAt.IsZero() {
			t.Errorf("job %d carries timestamps (%v, %v); the webhook has none to give",
				i, j.StartedAt, j.CompletedAt)
		}
	}
	if ci.GroupWeights(got) != nil {
		t.Error("untimed jobs must yield no weights, or the relay would start sizing pills")
	}
}

func TestStepRowsLabelsCapOmitsAboveTen(t *testing.T) {
	info := ci.StepInfo{TotalSteps: 11, StepRows: make([]int, 11), StepLabels: make([]string, 11)}
	rows, labels := stepRowsLabels(info)
	if rows != nil || labels != nil {
		t.Fatalf("expected nil rows/labels above the 10-group cap, got rows=%v labels=%v", rows, labels)
	}
}

func TestStepRowsLabelsClampsAndTruncates(t *testing.T) {
	rows := []int{15, 0, 3}
	labels := []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "short", "mid"} // first is 42 chars
	info := ci.StepInfo{TotalSteps: 3, StepRows: rows, StepLabels: labels}

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
