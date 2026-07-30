package ci

import (
	"testing"
	"time"
)

// testMaxWindow stands in for a caller's max-run-lifetime ceiling. Both pollers
// pass 12h.
const testMaxWindow = 12 * time.Hour

func TestLiveAnchor(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-30 * time.Second)
	weights := map[string]float64{"Build": 300}

	tests := []struct {
		name    string
		info    StepInfo
		weights map[string]float64
		wantOK  bool
		wantWhy AnchorDecline
		start   int64
		end     int64
	}{
		{
			name:    "anchors from the step's own start",
			info:    StepInfo{CurrentStep: 2, CurrentStepName: "Build", CurrentStepStartedAt: startedAt},
			weights: weights,
			wantOK:  true,
			start:   startedAt.Unix(),
			end:     startedAt.Unix() + 300,
		},
		{
			// Anchoring from poll time would animate a step that has not begun,
			// and would stay offset by up to a poll interval once the real start
			// appears, since the group has not changed and nothing re-anchors.
			name:    "no start stamped yet",
			info:    StepInfo{CurrentStep: 2, CurrentStepName: "Build"},
			weights: weights,
			wantWhy: DeclineNoStart,
		},
		{
			name:    "nothing running",
			info:    StepInfo{CurrentStep: 0},
			weights: weights,
			wantWhy: DeclineNoStepRunning,
		},
		{
			name:    "queued placeholder never matches a measured group",
			info:    StepInfo{CurrentStep: 2, CurrentStepName: "Queued", CurrentStepStartedAt: startedAt},
			weights: weights,
			wantWhy: DeclineUnmeasured,
		},
		{
			// The single-job smoke-test case: a workflow that has not finished on
			// this branch before has no measured group to animate toward. Nothing to
			// do with the job count, which is the wrong conclusion to draw from it.
			name:    "no prior run",
			info:    StepInfo{CurrentStep: 1, CurrentStepName: "Build", CurrentStepStartedAt: startedAt},
			wantOK:  false,
			wantWhy: DeclineUnmeasured,
		},
		{
			name:    "estimate already spent",
			info:    StepInfo{CurrentStep: 2, CurrentStepName: "Build", CurrentStepStartedAt: now.Add(-10 * time.Minute)},
			weights: weights,
			wantWhy: DeclineEstimateSpent,
		},
		{
			// GroupWeights seeds unmeasurable groups to the floor, so a floor
			// value is a pill width, not a duration. Anchoring on it would
			// fabricate a countdown out of a number nothing measured.
			name:    "floor weight is not a measurement",
			info:    StepInfo{CurrentStep: 2, CurrentStepName: "Build", CurrentStepStartedAt: startedAt},
			weights: map[string]float64{"Build": StepWeightFloor},
			wantWhy: DeclineUnmeasured,
		},
		{
			// A duration long enough to push end_date past the server's 5-year
			// ceiling would 422 every later patch and freeze the card.
			name:    "absurd duration is clamped to the tracking ceiling",
			info:    StepInfo{CurrentStep: 2, CurrentStepName: "Build", CurrentStepStartedAt: startedAt},
			weights: map[string]float64{"Build": 100 * 365 * 24 * 3600},
			wantOK:  true,
			start:   startedAt.Unix(),
			end:     startedAt.Unix() + int64(testMaxWindow.Seconds()),
		},
		{
			// The runner's clock reading ahead of ours would otherwise render an
			// empty bar until local time caught up.
			name:    "start in the future anchors from now",
			info:    StepInfo{CurrentStep: 2, CurrentStepName: "Build", CurrentStepStartedAt: now.Add(2 * time.Minute)},
			weights: weights,
			wantOK:  true,
			start:   now.Unix(),
			end:     now.Unix() + 300,
		},
		{
			name:    "window too short to be worth animating",
			info:    StepInfo{CurrentStep: 2, CurrentStepName: "Build", CurrentStepStartedAt: now.Add(-298 * time.Second)},
			weights: weights,
			wantWhy: DeclineEstimateSpent,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, end, ok, why := LiveAnchor(tc.info, tc.weights, now, testMaxWindow)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			// The reason is what a log line reports, so a decline naming the wrong
			// gate sends the next reader after the wrong cause.
			if why != tc.wantWhy {
				t.Errorf("why = %q, want %q", why, tc.wantWhy)
			}
			if !ok {
				return
			}
			if start != tc.start || end != tc.end {
				t.Errorf("window = (%d, %d), want (%d, %d)", start, end, tc.start, tc.end)
			}
		})
	}
}
