package widgets

import (
	"testing"
	"time"
)

func TestTrendSourcePushBuffersPoints(t *testing.T) {
	s := &TrendSource{repeatGap: time.Minute}
	now := time.Now()

	// Below the minimum there is no widget yet, so Points stays nil rather than
	// handing the manager a payload the server would reject.
	s.push(1, now)
	if got := s.Points(); got != nil {
		t.Errorf("expected no points below the minimum, got %v", got)
	}
	s.push(2, now)
	if got := s.Points(); len(got) != 2 {
		t.Fatalf("expected 2 points, got %v", got)
	}

	// The clone keeps the live buffer from being aliased into a payload that
	// outlives the tick.
	got := s.Points()
	got[0] = 99
	if s.points[0] == 99 {
		t.Error("Points aliased the live buffer")
	}
}

func TestTrendSourceDropsOldestAtCap(t *testing.T) {
	s := &TrendSource{repeatGap: time.Minute}
	now := time.Now()
	for i := range trendMaxPoints + 10 {
		s.push(float64(i), now.Add(time.Duration(i)*time.Hour))
	}
	pts := s.Points()
	if len(pts) != trendMaxPoints {
		t.Fatalf("expected the buffer capped at %d, got %d", trendMaxPoints, len(pts))
	}
	if pts[0] != 10 || pts[len(pts)-1] != float64(trendMaxPoints+9) {
		t.Errorf("expected the oldest samples dropped, got first=%v last=%v", pts[0], pts[len(pts)-1])
	}
}

func TestTrendSourceSuppressesFlatRepeats(t *testing.T) {
	s := &TrendSource{repeatGap: time.Minute}
	now := time.Now()
	s.push(5, now)
	s.push(5, now) // reaches the minimum, so this one still counts
	s.push(5, now.Add(10*time.Second))
	if len(s.points) != 2 {
		t.Errorf("expected a repeat inside the gap to be skipped, got %v", s.points)
	}
	s.push(5, now.Add(2*time.Minute))
	if len(s.points) != 3 {
		t.Errorf("expected a repeat past the gap to be recorded, got %v", s.points)
	}
}

func TestTrendRepeatGapFloor(t *testing.T) {
	// The floor is what stops a 5s interval flushing 48 minutes of real history
	// out of the buffer in four, leaving 48 identical dots.
	if got := trendRepeatGap(5 * time.Second); got != time.Minute {
		t.Errorf("expected a 60s floor, got %v", got)
	}
	if got := trendRepeatGap(5 * time.Minute); got != 5*time.Minute {
		t.Errorf("expected the interval above the floor, got %v", got)
	}
}
