package universal

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// Where Apply took a severity or lifecycle from.
const (
	FromTable     = "table"
	FromHeuristic = "heuristic"
	FromDefault   = "default"
)

// Event is one payload read through a mapping, ready to deliver.
type Event struct {
	Kind  Kind
	Title string
	Body  string
	URL   string
	// CorrelationKey is a hash of the correlation value; the value itself
	// never leaves Apply.
	CorrelationKey string
	// Progress is a fraction in [0, 1], nil when the payload has none.
	Progress *float64
	Severity string
	// Lifecycle is ongoing or ended; LifecycleRaw is the sample it came
	// from.
	Lifecycle    string
	LifecycleRaw string
	SevFrom      string
	LcFrom       string
}

// Apply reads one payload through a mapping. Severity and lifecycle go
// through the mapping's table, then the heuristic's own vocabulary, then a
// default; an unknown lifecycle value never ends anything. A sample that
// classifies as a secret is treated as missing, except that a value-only
// secret still serves as the correlation id, which is hashed and never shown.
// A ratio progress between 1 and 100 is read as a percentage. Title is never
// empty.
func Apply(m Mapping, fields []Field, source string) Event {
	byPath := make(map[string]*Field, len(m.Paths))
	for _, p := range m.Paths {
		byPath[p] = nil
	}
	for i := range fields {
		if f, ok := byPath[fields[i].Path]; ok && f == nil {
			byPath[fields[i].Path] = &fields[i]
		}
	}
	get := func(r Role) string {
		f := byPath[m.Paths[r]]
		if f == nil || ClassOf(f.Path, *f) == ClassSecret && (r != RoleCorrelation || secretPath(f.Path)) {
			return ""
		}
		return strings.TrimSpace(f.Value)
	}

	ev := Event{
		Kind:           m.Kind,
		Title:          get(RoleTitle),
		Body:           get(RoleBody),
		URL:            text.SanitizeURL(get(RoleURL)),
		CorrelationKey: correlationKey(get(RoleCorrelation)),
		LifecycleRaw:   get(RoleLifecycle),
	}
	switch ev.Kind {
	case KindNotification, KindAlert, KindProgress:
	default:
		ev.Kind = KindNotification
	}

	if raw := get(RoleProgress); raw != "" {
		if n, err := strconv.ParseFloat(raw, 64); err == nil {
			// A ratio mapping made from a first sample of 0 or 1 would
			// otherwise drop every later 45.
			if m.ProgressScale == ScalePercent || n > 1 && n <= 100 {
				n /= 100
			}
			if n >= 0 && n <= 1 && !math.IsNaN(n) {
				ev.Progress = &n
			}
		}
	}

	ev.Severity, ev.SevFrom = lookup(m.SeverityValues, get(RoleSeverity), severityStates, severityOf)
	if ev.Severity == "" {
		ev.Severity, ev.SevFrom = SeverityInfo, FromDefault
		if ev.Kind == KindAlert {
			ev.Severity = SeverityWarning
		}
	}
	ev.Lifecycle, ev.LcFrom = lookup(m.LifecycleValues, ev.LifecycleRaw, lifecycleStates, lifecycleOf)
	if ev.Lifecycle == "" {
		ev.Lifecycle, ev.LcFrom = LifecycleOngoing, FromDefault
	}

	if ev.Body == "" {
		ev.Body = ev.LifecycleRaw
	}
	if ev.Body == "" {
		ev.Body = "Event"
		if source != "" {
			ev.Body = "Event from " + source
		}
	}
	if ev.Title == "" {
		ev.Title = source
	}
	if ev.Title == "" {
		ev.Title = "Webhook"
	}
	return ev
}

func correlationKey(v string) string {
	if v == "" {
		return ""
	}
	return text.HashHex(v, 16)
}

// lookup resolves a sample through a value table, by its normalized form and
// then by its last word as the proposal keys are, and then through the
// heuristic's vocabulary.
func lookup(t map[string]string, raw string, states []string, heuristic func(string) string) (string, string) {
	if raw == "" {
		return "", ""
	}
	for _, k := range [2]string{normValue(raw), valueTail(raw)} {
		if v, ok := t[k]; ok && k != "" && slices.Contains(states, v) {
			return v, FromTable
		}
	}
	if v := heuristic(raw); v != "" {
		return v, FromHeuristic
	}
	return "", ""
}
