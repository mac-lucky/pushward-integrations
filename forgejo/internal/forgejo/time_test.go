package forgejo

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFlexTime(t *testing.T) {
	want := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		raw  string
		want time.Time
	}{
		{"null", `null`, time.Time{}},
		{"empty string", `""`, time.Time{}},
		{"rfc3339 utc", `"2026-07-30T10:00:00Z"`, want},
		{"rfc3339 offset", `"2026-07-30T12:00:00+02:00"`, want},
		{"bare unix seconds", `1785405600`, want},
		// Forgejo writes an unset time as the epoch rather than null. Taken
		// literally that gives an unstarted job a 1970 start, which becomes a
		// 55-year live-progress window or a 55-year pill.
		{"epoch means unset", `"1970-01-01T00:00:00Z"`, time.Time{}},
		{"epoch with offset means unset", `"1970-01-01T01:00:00+01:00"`, time.Time{}},
		{"zero unix means unset", `0`, time.Time{}},
		{"go zero time means unset", `"0001-01-01T00:00:00Z"`, time.Time{}},
		// A single bad value must not fail the whole page decode and drop a poll.
		{"garbage string", `"garbage"`, time.Time{}},
		{"wrong shape", `{"a":1}`, time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var f flexTime
			if err := json.Unmarshal([]byte(tc.raw), &f); err != nil {
				t.Fatalf("UnmarshalJSON(%s) returned an error: %v; it must never fail", tc.raw, err)
			}
			if !f.Time().Equal(tc.want) {
				t.Errorf("Time() = %v, want %v", f.Time(), tc.want)
			}
			if f.IsZero() != tc.want.IsZero() {
				t.Errorf("IsZero() = %v, want %v", f.IsZero(), tc.want.IsZero())
			}
		})
	}
}

// TestFlexTimeInStructSurvivesBadNeighbour is the property that matters in
// production: one unparseable timestamp cannot take the rest of the row with it.
func TestFlexTimeInStructSurvivesBadNeighbour(t *testing.T) {
	var task wireTask
	raw := `{"id": 7, "name": "build", "status": "success",
	         "run_started_at": "garbage", "updated_at": "2026-07-30T10:00:00Z"}`
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if task.ID != 7 || task.Name != "build" {
		t.Errorf("scalar fields lost: %+v", task)
	}
	if !task.RunStartedAt.IsZero() {
		t.Errorf("RunStartedAt = %v, want zero", task.RunStartedAt.Time())
	}
	if task.UpdatedAt.IsZero() {
		t.Error("UpdatedAt must still decode")
	}
}
