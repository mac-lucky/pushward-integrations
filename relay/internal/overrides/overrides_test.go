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
		"ack=",
		"ack=yes",
		"ack=1&ack_repeat=29",
		"ack=1&ack_repeat=3601",
		"ack=1&ack_repeat=abc",
		"ack=1&ack_expire=59",
		"ack=1&ack_expire=10801",
		"ack=1&ack_expire=",
		// The durations alone, or next to ack=0, would repeat nothing.
		"ack_repeat=60",
		"ack_expire=600",
		"ack=0&ack_repeat=60",
		// A passive notification does not alert, so it cannot repeat.
		"ack=1&level=passive",
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
	if !o.NotifyFallback(true) {
		t.Error("nil Overrides should notify for a worth-it event")
	}
	if o.NotifyFallback(false) {
		t.Error("nil Overrides allows the activity, so a routine event should not also push")
	}
}

func TestNotifyFallback(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		worthIt bool
		want    bool
	}{
		// No override: both surfaces allowed, so only worth-it events push and a
		// routine event stays activity-only.
		{"default, worth it", "", true, true},
		{"default, routine", "", false, false},
		// Activity suppressed: the notification is the only delivery left, so
		// even a routine event has to push.
		{"notification only, worth it", "channels=notification", true, true},
		{"notification only, routine", "channels=notification", false, true},
		// Notification suppressed: never push, however worth it.
		{"activity only, worth it", "channels=activity", true, false},
		{"activity only, routine", "channels=activity", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, err := Parse(mustQuery(t, tc.query))
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			if got := o.NotifyFallback(tc.worthIt); got != tc.want {
				t.Errorf("NotifyFallback(%v) = %v, want %v", tc.worthIt, got, tc.want)
			}
		})
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

func TestParseAck(t *testing.T) {
	tests := []struct {
		query          string
		repeat, expire int // 0, 0 = no ack
	}{
		{"", 0, 0},
		{"ack=0", 0, 0},
		{"ack=false", 0, 0},
		{"ack=1", DefaultAckRepeat, DefaultAckExpire},
		{"ack=true", DefaultAckRepeat, DefaultAckExpire},
		{"ack=1&ack_repeat=30", 30, DefaultAckExpire},
		{"ack=1&ack_expire=10800", DefaultAckRepeat, 10800},
		{"ack=1&ack_repeat=3600&ack_expire=60", 3600, 60},
		{"ack=1&level=critical", DefaultAckRepeat, DefaultAckExpire},
		{"ack=1&channels=activity", DefaultAckRepeat, DefaultAckExpire},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			o, err := Parse(mustQuery(t, tt.query))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			a := o.Ack()
			if tt.repeat == 0 {
				if a != nil {
					t.Fatalf("Ack() = %+v, want nil", *a)
				}
				return
			}
			if a == nil || a.RepeatSeconds != tt.repeat || a.ExpireSeconds != tt.expire {
				t.Fatalf("Ack() = %+v, want repeat %d expire %d", a, tt.repeat, tt.expire)
			}
		})
	}
}

func TestNilReceiverHasNoAck(t *testing.T) {
	var o *Overrides
	if o.Ack() != nil {
		t.Error("nil Overrides should carry no ack")
	}
}

func TestWithAck(t *testing.T) {
	// Without any overrides on the context: ack at its defaults, the rest
	// untouched.
	o := FromContext(WithAck(context.Background()))
	if a := o.Ack(); a == nil || a.RepeatSeconds != DefaultAckRepeat || a.ExpireSeconds != DefaultAckExpire {
		t.Fatalf("Ack() = %+v, want the defaults", a)
	}
	if !o.AllowsActivity() || !o.AllowsNotification() {
		t.Error("WithAck changed the channels")
	}

	// The query's other overrides survive, and so does an ack it set itself.
	parsed, err := Parse(mustQuery(t, "channels=notification&level=critical&ack=1&ack_repeat=45"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), contextKey{}, parsed)
	o = FromContext(WithAck(ctx))
	if o.AllowsActivity() || o.LevelOr("active") != "critical" {
		t.Error("WithAck dropped the query's channels or level")
	}
	if a := o.Ack(); a == nil || a.RepeatSeconds != 45 {
		t.Errorf("Ack() = %+v, want the query's repeat 45", a)
	}

	// The overrides on the original context are not modified.
	plain := &Overrides{}
	ctx = context.WithValue(context.Background(), contextKey{}, plain)
	_ = WithAck(ctx)
	if plain.Ack() != nil {
		t.Error("WithAck modified the overrides it was given")
	}
}
