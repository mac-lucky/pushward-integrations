// Package overrides parses and carries the per-request query-parameter overrides
// that let a webhook URL change a provider's delivery behavior for a single
// request: channels (which delivery surfaces may be used), priority (the
// CreateActivity priority), level (the notification interruption level), and
// ack with ack_repeat and ack_expire (alerts repeat until acknowledged). An
// explicit param always wins over provider-computed values and static config;
// absent params leave today's behavior unchanged.
package overrides

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Delivery-surface names accepted by the channels param.
const (
	ChannelActivity     = "activity"
	ChannelNotification = "notification"
)

// Overrides holds the parsed overrides for one request. Every accessor is
// nil-safe, so a nil *Overrides behaves as "no overrides".
type Overrides struct {
	// channels is the set of allowed delivery surfaces. A nil map means the
	// param was absent, so every surface is allowed. A non-nil map lists the
	// permitted surfaces; a surface not in it is suppressed.
	channels map[string]bool
	// Priority overrides the static per-provider CreateActivity priority when non-nil.
	Priority *int
	// Level overrides the notification interruption level when non-empty.
	Level string
	// ack is set when the request asked for acknowledged alerts (ack=1).
	ack *Ack
}

// Ack is the acknowledge override: an alert notification repeats every
// RepeatSeconds until someone acknowledges it or ExpireSeconds pass.
type Ack struct {
	RepeatSeconds int
	ExpireSeconds int
}

// Acknowledge defaults and the server's bounds for them.
const (
	DefaultAckRepeat = 300
	DefaultAckExpire = 3600

	minAckRepeat, maxAckRepeat = 30, 3600
	minAckExpire, maxAckExpire = 60, 10800
)

// Ack returns the acknowledge override, or nil when the request did not ask
// for one.
func (o *Overrides) Ack() *Ack {
	if o == nil {
		return nil
	}
	return o.ack
}

// AllowsActivity reports whether Live Activity calls (create/update/end) are
// permitted for this request.
func (o *Overrides) AllowsActivity() bool {
	return o == nil || o.channels == nil || o.channels[ChannelActivity]
}

// AllowsNotification reports whether push notifications are permitted for this request.
func (o *Overrides) AllowsNotification() bool {
	return o == nil || o.channels == nil || o.channels[ChannelNotification]
}

// NotifyFallback reports whether an event should also raise a push
// notification. worthIt is the provider's own judgement that the event is worth
// interrupting for; on top of that, anything notifies once the activity surface
// is suppressed, because the push is then the only delivery left.
//
// This is only about the notification surface. Gating an activity update on it
// inverts the second term.
func (o *Overrides) NotifyFallback(worthIt bool) bool {
	return o.AllowsNotification() && (worthIt || !o.AllowsActivity())
}

// PriorityOr returns the priority override when set, otherwise def.
func (o *Overrides) PriorityOr(def int) int {
	if o != nil && o.Priority != nil {
		return *o.Priority
	}
	return def
}

// LevelOr returns the level override when set, otherwise def.
func (o *Overrides) LevelOr(def string) string {
	if o != nil && o.Level != "" {
		return o.Level
	}
	return def
}

// validLevels matches the pushward.Level* constants.
var validLevels = map[string]bool{
	"passive":        true,
	"active":         true,
	"time-sensitive": true,
	"critical":       true,
}

// Parse reads and validates the channels / priority / level / ack query
// params. A missing param leaves its field at the zero value. Any invalid
// value returns an error suitable for a 400 response.
func Parse(q url.Values) (*Overrides, error) {
	o := &Overrides{}

	if q.Has("channels") {
		set := make(map[string]bool)
		for _, part := range strings.Split(q.Get("channels"), ",") {
			c := strings.TrimSpace(part)
			if c == "" {
				continue
			}
			if c != ChannelActivity && c != ChannelNotification {
				return nil, fmt.Errorf("invalid channels value %q: allowed values are activity, notification", c)
			}
			set[c] = true
		}
		if len(set) == 0 {
			return nil, fmt.Errorf("channels must list at least one of activity, notification")
		}
		o.channels = set
	}

	if q.Has("priority") {
		raw := strings.TrimSpace(q.Get("priority"))
		p, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid priority %q: must be an integer 0-10", raw)
		}
		if p < 0 || p > 10 {
			return nil, fmt.Errorf("invalid priority %d: must be 0-10", p)
		}
		o.Priority = &p
	}

	if q.Has("level") {
		l := strings.TrimSpace(q.Get("level"))
		if !validLevels[l] {
			return nil, fmt.Errorf("invalid level %q: must be one of passive, active, time-sensitive, critical", l)
		}
		o.Level = l
	}

	if err := parseAck(q, o); err != nil {
		return nil, err
	}
	return o, nil
}

// parseAck reads ack, ack_repeat and ack_expire into o. The durations mean
// nothing without ack, and a passive notification does not alert, so it has
// nothing to repeat. Both are refused rather than ignored: a typo in the URL
// is then a 400, not an alert that quietly never repeats.
func parseAck(q url.Values, o *Overrides) error {
	on := false
	if q.Has("ack") {
		raw := strings.TrimSpace(q.Get("ack"))
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("invalid ack %q: must be 1 or 0", raw)
		}
		on = b
	}
	if !on {
		for _, k := range []string{"ack_repeat", "ack_expire"} {
			if q.Has(k) {
				return fmt.Errorf("%s needs ack=1", k)
			}
		}
		return nil
	}
	if o.Level == "passive" {
		return fmt.Errorf("ack=1 cannot be combined with level=passive: a passive notification does not alert, so it cannot repeat")
	}
	repeat, err := seconds(q, "ack_repeat", DefaultAckRepeat, minAckRepeat, maxAckRepeat)
	if err != nil {
		return err
	}
	expire, err := seconds(q, "ack_expire", DefaultAckExpire, minAckExpire, maxAckExpire)
	if err != nil {
		return err
	}
	o.ack = &Ack{RepeatSeconds: repeat, ExpireSeconds: expire}
	return nil
}

// seconds reads the integer param k, def when it is absent.
func seconds(q url.Values, k string, def, lo, hi int) (int, error) {
	if !q.Has(k) {
		return def, nil
	}
	raw := strings.TrimSpace(q.Get(k))
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: must be an integer %d-%d", k, raw, lo, hi)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("invalid %s %d: must be %d-%d", k, n, lo, hi)
	}
	return n, nil
}

type contextKey struct{}

// ContextKey returns the context key used to store Overrides, so middleware in
// another package can store it with a matching key.
func ContextKey() any { return contextKey{} }

// FromContext returns the Overrides stored on ctx, or a zero-value Overrides
// (all surfaces allowed, no overrides) when none is present. The result is
// never nil, so handlers can chain accessors without a guard.
func FromContext(ctx context.Context) *Overrides {
	if o, ok := ctx.Value(contextKey{}).(*Overrides); ok && o != nil {
		return o
	}
	return &Overrides{}
}

// WithAck returns ctx with the request's overrides plus acknowledge at its
// defaults, unless the query already asked for it. It is for a route that
// opts in by its path, for a sender that cannot add a query string.
func WithAck(ctx context.Context) context.Context {
	o := *FromContext(ctx)
	if o.ack == nil {
		o.ack = &Ack{RepeatSeconds: DefaultAckRepeat, ExpireSeconds: DefaultAckExpire}
	}
	return context.WithValue(ctx, contextKey{}, &o)
}
