package poller

import (
	"testing"
	"time"

	ghclient "github.com/mac-lucky/pushward-integrations/github/internal/github"
	"github.com/mac-lucky/pushward-integrations/shared/ci"
)

func TestParseJobTime(t *testing.T) {
	want := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	tests := []struct {
		name string
		in   string
		want time.Time
	}{
		{"absent", "", time.Time{}},
		{"malformed", "not-a-timestamp", time.Time{}},
		{"date only", "2026-01-01", time.Time{}},
		{"rfc3339 utc", "2026-01-01T00:00:10Z", want},
		{"rfc3339 offset", "2026-01-01T01:00:10+01:00", want},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseJobTime(tc.in)
			if !got.Equal(tc.want) {
				t.Errorf("parseJobTime(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestToCIJobs(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	end := time.Date(2026, 1, 1, 0, 5, 10, 0, time.UTC)

	got := toCIJobs([]ghclient.Job{
		{
			Name: "Lint", Status: "completed", Conclusion: "success",
			StartedAt: start.Format(time.RFC3339), CompletedAt: end.Format(time.RFC3339),
		},
		{Name: "Build", Status: "in_progress", StartedAt: start.Format(time.RFC3339)},
		{Name: "Deploy", Status: "queued"},
		// An unparseable stamp must not fabricate an anchor: the ladder reads a
		// zero time as "unknown" and falls back to the static bar.
		{Name: "Docs", Status: "in_progress", StartedAt: "not-a-timestamp"},
	})

	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	if got[0].Name != "Lint" || got[0].Status != ci.StatusCompleted || got[0].Conclusion != ci.ConclusionSuccess {
		t.Errorf("job 0 = %+v", got[0])
	}
	if !got[0].StartedAt.Equal(start) || !got[0].CompletedAt.Equal(end) {
		t.Errorf("job 0 times = (%v, %v), want (%v, %v)", got[0].StartedAt, got[0].CompletedAt, start, end)
	}
	if !got[1].StartedAt.Equal(start) || !got[1].CompletedAt.IsZero() {
		t.Errorf("a running job must carry a start and no completion, got (%v, %v)", got[1].StartedAt, got[1].CompletedAt)
	}
	if !got[2].StartedAt.IsZero() || !got[2].CompletedAt.IsZero() {
		t.Errorf("a queued job must carry no times, got (%v, %v)", got[2].StartedAt, got[2].CompletedAt)
	}
	if !got[3].StartedAt.IsZero() {
		t.Errorf("an unparseable start must stay zero, got %v", got[3].StartedAt)
	}

	if toCIJobs(nil) == nil {
		t.Error("toCIJobs(nil) must return an empty slice, not nil")
	}
}

// TestLiveAnchor_FlagOff is the half of the old table-driven liveAnchor test
// that exercised the config gate rather than the shared window arithmetic; the
// rest now lives in shared/ci.
func TestLiveAnchor_FlagOff(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	info := ci.StepInfo{
		CurrentStep:          2,
		CurrentStepName:      "Build",
		CurrentStepStartedAt: now.Add(-30 * time.Second),
	}
	weights := map[string]float64{"Build": 300}

	cfg := testConfig()
	cfg.Render.LiveProgress = false
	if _, _, ok := (&Poller{cfg: cfg}).liveAnchor(info, weights, now); ok {
		t.Error("expected no anchor when live progress is disabled")
	}

	cfg = testConfig()
	cfg.Render.LiveProgress = true
	if _, _, ok := (&Poller{cfg: cfg}).liveAnchor(info, weights, now); !ok {
		t.Error("expected an anchor when live progress is enabled")
	}
}
