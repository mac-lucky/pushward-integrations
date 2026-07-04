package overrides

import (
	"context"
	"net/url"
	"testing"
)

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", raw, err)
	}
	return v
}

func TestParseValid(t *testing.T) {
	tests := []struct {
		name         string
		query        string
		wantActivity bool
		wantNotify   bool
		wantPriority int // PriorityOr(7)
		wantLevel    string
	}{
		{"absent", "", true, true, 7, "passive"},
		{"channels notification only", "channels=notification", false, true, 7, "passive"},
		{"channels activity only", "channels=activity", true, false, 7, "passive"},
		{"channels both", "channels=activity,notification", true, true, 7, "passive"},
		{"channels tolerates spacing and trailing comma", "channels=activity,%20", true, false, 7, "passive"},
		{"priority override", "priority=3", true, true, 3, "passive"},
		{"priority zero", "priority=0", true, true, 0, "passive"},
		{"priority ten", "priority=10", true, true, 10, "passive"},
		{"level override", "level=critical", true, true, 7, "critical"},
		{"all three", "channels=notification&priority=8&level=time-sensitive", false, true, 8, "time-sensitive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, err := Parse(mustQuery(t, tt.query))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := o.AllowsActivity(); got != tt.wantActivity {
				t.Errorf("AllowsActivity() = %v, want %v", got, tt.wantActivity)
			}
			if got := o.AllowsNotification(); got != tt.wantNotify {
				t.Errorf("AllowsNotification() = %v, want %v", got, tt.wantNotify)
			}
			if got := o.PriorityOr(7); got != tt.wantPriority {
				t.Errorf("PriorityOr(7) = %d, want %d", got, tt.wantPriority)
			}
			if got := o.LevelOr("passive"); got != tt.wantLevel {
				t.Errorf("LevelOr(passive) = %q, want %q", got, tt.wantLevel)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	for _, q := range []string{
		"channels=",
		"channels=,",
		"channels=bogus",
		"channels=activity,bogus",
		"priority=11",
		"priority=-1",
		"priority=abc",
		"priority=",
		"level=bogus",
		"level=",
	} {
		t.Run(q, func(t *testing.T) {
			if _, err := Parse(mustQuery(t, q)); err == nil {
				t.Errorf("Parse(%q) = nil error, want error", q)
			}
		})
	}
}

func TestNilReceiverIsDefault(t *testing.T) {
	var o *Overrides
	if !o.AllowsActivity() || !o.AllowsNotification() {
		t.Error("nil Overrides should allow every surface")
	}
	if o.PriorityOr(4) != 4 {
		t.Error("nil Overrides should return the default priority")
	}
	if o.LevelOr("active") != "active" {
		t.Error("nil Overrides should return the default level")
	}
}

func TestFromContextDefault(t *testing.T) {
	o := FromContext(context.Background())
	if o == nil {
		t.Fatal("FromContext returned nil")
	}
	if !o.AllowsActivity() || !o.AllowsNotification() {
		t.Error("absent Overrides should allow every surface")
	}
}

func TestFromContextRoundTrip(t *testing.T) {
	want, err := Parse(mustQuery(t, "channels=notification&priority=9"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), contextKey{}, want)
	got := FromContext(ctx)
	if got.AllowsActivity() || !got.AllowsNotification() {
		t.Error("round-tripped channels lost")
	}
	if got.PriorityOr(0) != 9 {
		t.Error("round-tripped priority lost")
	}
}
