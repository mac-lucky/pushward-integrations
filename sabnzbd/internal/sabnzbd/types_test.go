package sabnzbd

import "testing"

func TestQueuePropagating(t *testing.T) {
	tests := []struct {
		name  string
		slots []QueueSlot
		want  int
	}{
		{"no slots", nil, 0},
		{"none propagating", []QueueSlot{{Status: "Queued"}, {Status: "Downloading"}}, 0},
		{"one of two", []QueueSlot{{Status: "Propagating"}, {Status: "Queued"}}, 1},
		{"all propagating", []QueueSlot{{Status: "Propagating"}, {Status: "Propagating"}}, 2},
		// SABnzbd always sends status, but an absent field decodes to "".
		{"absent status", []QueueSlot{{Status: ""}}, 0},
		// The translated label is not the status, which is why we match on the latter.
		{"label form is not the status", []QueueSlot{{Status: "PROPAGATING 3 min"}}, 0},
		{"lowercase is not the status", []QueueSlot{{Status: "propagating"}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &Queue{Slots: tt.slots}
			if got := q.Propagating(); got != tt.want {
				t.Errorf("Propagating() = %d, want %d", got, tt.want)
			}
		})
	}
}
