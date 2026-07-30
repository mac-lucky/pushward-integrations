package forgejo

import (
	"slices"
	"testing"

	"github.com/mac-lucky/pushward-integrations/shared/ci"
)

func TestNormalizeStatus(t *testing.T) {
	tests := []struct {
		in             string
		wantStatus     string
		wantConclusion string
	}{
		{StatusSuccess, ci.StatusCompleted, ci.ConclusionSuccess},
		{StatusFailure, ci.StatusCompleted, ci.ConclusionFailure},
		{StatusCancelled, ci.StatusCompleted, ci.ConclusionCancelled},
		{StatusSkipped, ci.StatusCompleted, ci.ConclusionSkipped},
		{StatusRunning, ci.StatusInProgress, ""},
		{StatusWaiting, ci.StatusQueued, ""},
		{StatusBlocked, ci.StatusQueued, ""},
		{StatusUnknown, ci.StatusQueued, ""},
		// A value a later Forgejo release might add must read as pending. Reading
		// it as completed would dismiss a Live Activity for a run still going.
		{"some_future_state", ci.StatusQueued, ""},
		{"", ci.StatusQueued, ""},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			status, conclusion := normalizeStatus(tc.in)
			if status != tc.wantStatus || conclusion != tc.wantConclusion {
				t.Errorf("normalizeStatus(%q) = (%q, %q), want (%q, %q)",
					tc.in, status, conclusion, tc.wantStatus, tc.wantConclusion)
			}
		})
	}
}

// TestNormalizeStatusSynthesisesNoExtraConclusions pins that only the four
// terminal statuses produce a conclusion. Forgejo has no conclusion field, so
// every value here is manufactured and an accidental extra one would make a
// running job look decided.
func TestNormalizeStatusSynthesisesNoExtraConclusions(t *testing.T) {
	terminal := []string{StatusSuccess, StatusFailure, StatusCancelled, StatusSkipped}
	all := []string{
		StatusUnknown, StatusWaiting, StatusRunning, StatusSuccess,
		StatusFailure, StatusCancelled, StatusSkipped, StatusBlocked,
	}
	for _, s := range all {
		_, conclusion := normalizeStatus(s)
		wantSet := slices.Contains(terminal, s)
		if (conclusion != "") != wantSet {
			t.Errorf("normalizeStatus(%q) conclusion = %q; a conclusion must be set for exactly the terminal statuses", s, conclusion)
		}
	}
}

// TestJobFailedAgreesWithNormalize checks the mapping lands where the shared
// ladder expects: failure and cancelled redden a run, skipped does not.
func TestJobFailedAgreesWithNormalize(t *testing.T) {
	wantFailed := map[string]bool{
		StatusSuccess:   false,
		StatusFailure:   true,
		StatusCancelled: true,
		StatusSkipped:   false,
		StatusRunning:   false,
		StatusWaiting:   false,
		StatusBlocked:   false,
		StatusUnknown:   false,
	}
	for status, want := range wantFailed {
		_, conclusion := normalizeStatus(status)
		if got := ci.JobFailed(conclusion); got != want {
			t.Errorf("status %q -> conclusion %q -> JobFailed=%v, want %v", status, conclusion, got, want)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	cases := map[string]bool{
		StatusSuccess:   true,
		StatusFailure:   true,
		StatusCancelled: true,
		StatusSkipped:   true,
		StatusRunning:   false,
		StatusWaiting:   false,
		StatusBlocked:   false,
		StatusUnknown:   false,
		"":              false,
	}
	for status, want := range cases {
		if got := isTerminal(status); got != want {
			t.Errorf("isTerminal(%q) = %v, want %v", status, got, want)
		}
	}
}

// TestIsTerminalMatchesNormalizeStatus keeps the two ways of asking "has this
// stopped?" in step. Run.Terminal reads Forgejo's raw status through isTerminal,
// while the shared poller reads the normalized one and checks it against
// ci.StatusCompleted. A status added to one and not the other would end a card on
// one path and leave it running on the other.
func TestIsTerminalMatchesNormalizeStatus(t *testing.T) {
	statuses := []string{
		StatusUnknown, StatusWaiting, StatusRunning, StatusSuccess, StatusFailure,
		StatusCancelled, StatusSkipped, StatusBlocked,
		"", "a-status-a-later-release-adds",
	}
	for _, raw := range statuses {
		status, _ := normalizeStatus(raw)
		if got, want := status == ci.StatusCompleted, isTerminal(raw); got != want {
			t.Errorf("status %q: normalizeStatus says completed=%v but isTerminal says %v", raw, got, want)
		}
	}
}

// TestFinishedStatusSetsCoverTerminal keeps the two-pass seed lookup in step
// with isTerminal. Forgejo has no "completed" umbrella filter value, so the
// second pass enumerates the rest by hand and would silently stop finding runs
// if a status were added to one place and not the other.
func TestFinishedStatusSetsCoverTerminal(t *testing.T) {
	if len(finishedStatusSets) != 2 {
		t.Fatalf("expected 2 passes, got %d", len(finishedStatusSets))
	}
	if !slices.Equal(finishedStatusSets[0], []string{StatusSuccess}) {
		t.Errorf("the first pass must ask for success alone, got %v", finishedStatusSets[0])
	}

	var union []string
	for _, set := range finishedStatusSets {
		union = append(union, set...)
	}
	for _, s := range union {
		if !isTerminal(s) {
			t.Errorf("seed lookup asks for %q, which isTerminal rejects", s)
		}
	}
	for _, s := range []string{StatusSuccess, StatusFailure, StatusCancelled, StatusSkipped} {
		if !slices.Contains(union, s) {
			t.Errorf("terminal status %q is never asked for, so those runs can never seed a shape", s)
		}
	}
}

// TestActiveStatusesExcludeBlocked documents the deliberate omission: a run held
// for approval may never execute, and tracking it would strand a card that only
// the 12-hour lifetime guard could reclaim.
func TestActiveStatusesExcludeBlocked(t *testing.T) {
	if slices.Contains(activeStatuses, StatusBlocked) {
		t.Error("blocked runs must not be tracked")
	}
	for _, s := range []string{StatusRunning, StatusWaiting} {
		if !slices.Contains(activeStatuses, s) {
			t.Errorf("the idle probe must ask for %q", s)
		}
	}
}
