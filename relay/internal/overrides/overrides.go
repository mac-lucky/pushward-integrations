// Package overrides parses and carries the per-request query-parameter overrides
// that let a webhook URL change a provider's delivery behavior for a single
// request. Three params are supported: channels (which delivery surfaces may be
// used), priority (the CreateActivity priority), and level (the notification
// interruption level). An explicit param always wins over provider-computed
// values and static config; absent params leave today's behavior unchanged.
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

// Parse reads and validates the channels / priority / level query params. A
// missing param leaves its field at the zero value. Any invalid value returns
// an error suitable for a 400 response.
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

	return o, nil
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
