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
			// Without the explicit guard the mean fallback would happily count the
			// placeholder down, and the log would blame the wrong gate.
			name:    "queued placeholder never matches a measured group",
			info:    StepInfo{CurrentStep: 2, CurrentStepName: QueuedStepName, CurrentStepStartedAt: startedAt},
			weights: weights,
			wantWhy: DeclineQueued,
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
			// GroupWeights seeds unmeasurable groups to the floor, so a floor value
			// is a pill width, not a duration - and here it is the ONLY value, so
			// the run measured nothing and there is no mean to fall back to either.
			name:    "floor weight is not a measurement",
			info:    StepInfo{CurrentStep: 2, CurrentStepName: "Build", CurrentStepStartedAt: startedAt},
			weights: map[string]float64{"Build": StepWeightFloor},
			wantWhy: DeclineUnmeasured,
		},
		{
			// The Forgejo case: its durations come from a lossy tasks join, so most
			// groups come back at the floor while a couple are measured. The pill is
			// already drawn at the mean; the ETA now agrees with it instead of
			// leaving the card static for the whole run.
			name: "unmeasured group animates toward the mean of the run",
			info: StepInfo{
				CurrentStep: 2, CurrentStepName: "build",
				CurrentStepStartedAt: now.Add(-5 * time.Second),
			},
			weights: map[string]float64{
				"build": StepWeightFloor, "unit-tests": StepWeightFloor,
				"nginx-behaviour": 77, "unit-tests-1": 55,
			},
			wantOK: true,
			start:  now.Add(-5 * time.Second).Unix(),
			// (1 + 1 + 77 + 55) / 4 = 33.5, rounded to 34.
			end: now.Add(-5*time.Second).Unix() + 34,
		},
		{
			// A group the prior run never revealed at all - a job added since - gets
			// the same neutral estimate rather than a static bar.
			name:    "group absent from the prior run falls back too",
			info:    StepInfo{CurrentStep: 2, CurrentStepName: "Deploy", CurrentStepStartedAt: startedAt},
			weights: map[string]float64{"Build": 300, "Test": 100},
			wantOK:  true,
			start:   startedAt.Unix(),
			end:     startedAt.Unix() + 200,
		},
		{
			// The fallback is an estimate, not an excuse to skip the ceiling. The
			// multi-entry map matters: it makes the mean a value no direct lookup
			// could have produced, so this cannot pass on the measured path.
			name: "mean fallback is clamped to the tracking ceiling",
			info: StepInfo{CurrentStep: 2, CurrentStepName: "Deploy", CurrentStepStartedAt: startedAt},
			weights: map[string]float64{
				"Build": 200 * 365 * 24 * 3600, "Test": 100,
			},
			wantOK: true,
			start:  startedAt.Unix(),
			end:    startedAt.Unix() + int64(testMaxWindow.Seconds()),
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
