package pushward

import (
	"strings"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// Activity state constants.
const (
	StateOngoing = "ongoing"
	StateEnded   = "ended"
)

// Template name constants.
const (
	TemplateGeneric   = "generic"
	TemplateAlert     = "alert"
	TemplateSteps     = "steps"
	TemplateCountdown = "countdown"
	TemplateGauge     = "gauge"
	TemplateTimeline  = "timeline"
	TemplateBoard     = "board"
	TemplateLog       = "log"
	TemplateMedia     = "media"
	TemplateApproval  = "approval"
)

// Steps-template wire bounds, mirroring the server's per-entry validation.
//
// The server checks these per ENTRY and rejects the WHOLE payload on the first
// violation. Since a poller reuses one slug across runs, that rejection takes the
// step counter, the live window and both end frames with it: the card freezes on
// whatever frame last succeeded, and keeps freezing until some run happens to
// produce a shape that validates.
const (
	MaxStepLabelLen = 32 // runes per step_labels entry
	MinStepRows     = 1  // jobs per step_rows entry
	MaxStepRows     = 10
)

// MaxSeverityLabelRunes caps content.severity_label, which the server reads on
// the alert template only. Raised from 32 in server v1.11.0.
const MaxSeverityLabelRunes = 40

// DismissalTTLMax mirrors the server's dismissal_ttl ceiling: iOS will not hold
// an ended Live Activity on the Lock Screen longer than 4h.
const DismissalTTLMax = 14400

// SeverityLabel trims and clamps a badge string to the wire bound, the way
// ClampStepShape does for the steps trio: one place applies the limit so a new
// emitter cannot ship a label the server rejects. Every severity_label an
// integration sends goes through here.
func SeverityLabel(s string) string {
	return text.TruncateHard(strings.TrimSpace(s), MaxSeverityLabelRunes)
}

// ClampStepShape returns rows and labels clamped to the wire bounds above. It is
// the one place those bounds are applied, so a new emitter cannot ship a shape
// the server will reject.
//
// Call it at the assignment to the payload and nowhere earlier. Callers key their
// own bookkeeping on the FULL label - name-keyed weight lookups, step realignment
// between two label lists, the tracked shape - and truncating upstream would make
// every long-named group miss its own entry. Clamping rows early would corrupt the
// fan-out count the same way.
//
// Two labels that differ only past the bound clamp to the same text. They stay two
// entries: the clamp is cosmetic, and folding them would under-count total_steps
// and mis-attribute progress.
func ClampStepShape(rows []int, labels []string) ([]int, []string) {
	outRows := make([]int, len(rows))
	for i, r := range rows {
		outRows[i] = min(max(r, MinStepRows), MaxStepRows)
	}
	outLabels := make([]string, len(labels))
	for i, l := range labels {
		outLabels[i] = text.TruncateHard(l, MaxStepLabelLen)
	}
	return outRows, outLabels
}

// ImageShape names the frame the iOS client draws an activity's poster image
// in. Mirrors the server's image shape enum; an omitted value is stored
// verbatim and rendered as ImageShapeSquare.
type ImageShape string

const (
	ImageShapePoster ImageShape = "poster" // 2:3 portrait, e.g. movie/series art
	ImageShapeSquare ImageShape = "square" // 1:1, the default when omitted
	ImageShapeCircle ImageShape = "circle" // 1:1 clipped to a circle, e.g. avatars
)

// Trend direction constants annotate a board tile (and value/gauge widget)
// with a directional arrow. Mirrors the server's TrendDirection enum.
const (
	TrendUp   = "up"
	TrendDown = "down"
	TrendFlat = "flat"
)

// Log level constants tag an individual log-template line. Mirrors the
// server's LogLevel enum.
const (
	LogInfo  = "info"
	LogWarn  = "warn"
	LogError = "error"
)

// PlaybackState is the transport state of a media template. Mirrors the
// server's playback state enum; an omitted value defaults to
// PlaybackPaused server-side. Only PlaybackPlaying ticks the position on
// the device; the other three freeze the bar at PositionSeconds.
type PlaybackState string

const (
	PlaybackPlaying   PlaybackState = "playing"
	PlaybackPaused    PlaybackState = "paused"
	PlaybackStopped   PlaybackState = "stopped"
	PlaybackBuffering PlaybackState = "buffering"
)

// Notification interruption level constants.
const (
	LevelActive        = "active"
	LevelPassive       = "passive"
	LevelTimeSensitive = "time-sensitive"
	LevelCritical      = "critical"
)

// ActivitySound is a Live Activity alert-sound identifier. The typed alias
// stops the value being confused with other string arguments at call sites.
// Any string is accepted by the SDK - the server is the source of truth for
// the allowlist (it returns 400 on unrecognised values), so clients don't
// mirror it and avoid drift as new sounds are added server-side.
type ActivitySound string

// BoolPtr returns a pointer to the given bool value.
func BoolPtr(v bool) *bool { return &v }

// IntPtr returns a pointer to the given int value.
func IntPtr(v int) *int { return &v }

// Int64Ptr returns a pointer to the given int64 value.
func Int64Ptr(v int64) *int64 { return &v }

// Float64Ptr returns a pointer to the given float64 value.
func Float64Ptr(v float64) *float64 { return &v }

// DurationPtr returns a pointer to the given duration.
func DurationPtr(v time.Duration) *time.Duration { return &v }

// StringPtr returns a pointer to the given string value.
func StringPtr(v string) *string { return &v }

// HistoryPoint is a single timestamped value in a timeline series.
type HistoryPoint struct {
	Timestamp int64   `json:"timestamp"` // Unix timestamp (seconds)
	Value     float64 `json:"value"`     // Value
}

// Threshold defines a horizontal reference line on a timeline sparkline.
type Threshold struct {
	Value float64 `json:"value"`
	Color string  `json:"color,omitempty"`
	Label string  `json:"label,omitempty"`
}

// TapAction is a routed tap target on a Live Activity (or widget). It mirrors
// the server's model.TapAction and NotificationAction routing fields, so iOS
// dispatches a tap the same way across notifications and Live Activities. The
// behavior is inferred from the URL scheme + Foreground flag:
//   - custom scheme (e.g. youtube://, homeassistant://) -> opens that app
//   - http(s) + Foreground=true -> opens the URL in Safari / in-app browser
//   - http(s) + Foreground=false -> silent webhook (Method/Headers/Body honored)
//
// Title and Icon are only meaningful when the action is rendered as a button
// (url_action / secondary_url_action); the widget-wide tap_action ignores them.
type TapAction struct {
	URL        string            `json:"url"`
	Foreground bool              `json:"foreground,omitempty"`
	Method     string            `json:"method,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	Title      string            `json:"title,omitempty"`
	Icon       string            `json:"icon,omitempty"`
}

// BoardTile is a single cell in a board template (1-4 per activity). Value is a
// string so callers can render non-numeric states ("Open", "On") alongside
// numbers. Trend is one of TrendUp/TrendDown/TrendFlat. URLAction reuses the
// shared TapAction routing type so a tile tap behaves like any other target.
type BoardTile struct {
	Label     string     `json:"label"`
	Value     string     `json:"value"`
	Unit      string     `json:"unit,omitempty"`
	Icon      string     `json:"icon,omitempty"`
	Color     string     `json:"color,omitempty"`
	Trend     string     `json:"trend,omitempty"`
	URLAction *TapAction `json:"url_action,omitempty"`
}

// LogLine is a single entry in a log template (1-20 per activity, newest-first).
// At is an optional unix timestamp (seconds); Level is an optional severity tag
// (LogInfo/LogWarn/LogError). The server accumulates a rolling backlog of lines
// server-side; that backlog is read-only and never sent by clients.
type LogLine struct {
	Text  string `json:"text"`
	At    *int64 `json:"at,omitempty"`
	Level string `json:"level,omitempty"`
}

// MediaControls are the transport buttons of a media template. Every slot is
// optional and reuses TapAction. iOS picks Play or Pause by PlaybackState and
// falls back to PlayPause (a toggle endpoint) when the split pair is absent;
// Favorite is the heart, VolumeDown/VolumeUp bracket the volume bar. Extra
// holds up to 3 more buttons and each of them needs an Icon.
//
// An http(s) slot is always a silent webhook: the server fills an empty Method
// with POST at validation (stored and returned) and answers 422 to
// Foreground=true. A custom scheme opens that app instead. The named slots
// draw a fixed glyph, so Title and Icon matter only on Extra.
type MediaControls struct {
	Previous   *TapAction  `json:"previous,omitempty"`
	PlayPause  *TapAction  `json:"play_pause,omitempty"`
	Play       *TapAction  `json:"play,omitempty"`
	Pause      *TapAction  `json:"pause,omitempty"`
	Next       *TapAction  `json:"next,omitempty"`
	Stop       *TapAction  `json:"stop,omitempty"`
	Favorite   *TapAction  `json:"favorite,omitempty"`
	VolumeDown *TapAction  `json:"volume_down,omitempty"`
	VolumeUp   *TapAction  `json:"volume_up,omitempty"`
	Extra      []TapAction `json:"extra,omitempty"`
}

// ApprovalStyle is the rendering weight of one approval option button.
// Omitted, the first option renders primary and the rest secondary.
type ApprovalStyle string

// Approval option styles.
const (
	ApprovalStylePrimary     ApprovalStyle = "primary"
	ApprovalStyleSecondary   ApprovalStyle = "secondary"
	ApprovalStyleDestructive ApprovalStyle = "destructive"
)

// ApprovalOption is one answer button of the approval template (2-4 per
// activity). It carries the TapAction routing fields plus the button identity:
// a stable ID (unique within the options, slug charset, at most 64 chars), a
// required Title (at most 24 runes) and an optional Style and Icon (Icon
// becomes required once the activity has three or more options - those render
// as icon-first tiles). An http(s) URL is always a silent webhook: the server
// rejects a foreground shape and fills an empty Method with POST, like a
// media control.
//
// Omit URL entirely for the server-recorded form: the server fills the option
// with a signed answer URL of its own, and the first tap is written to
// Content.Answer, pushed to every device, and ends the activity a few seconds
// later (DismissalTTL controls how long the answered card lingers on the Lock
// Screen). Mixing both forms in one activity is allowed; a tap on an option
// with your own URL is never recorded in Answer.
type ApprovalOption struct {
	ID      string            `json:"id"`
	Title   string            `json:"title"`
	Style   ApprovalStyle     `json:"style,omitempty"`
	Icon    string            `json:"icon,omitempty"`
	URL     string            `json:"url,omitempty"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// ApprovalDetail is one label/value context row shown between the question
// and the buttons (recipient, amount, environment). At most 2 rows; Label at
// most 24 runes, Value at most 64.
type ApprovalDetail struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// ApprovalAnswer is the server-recorded resolution of an approval activity.
// Read-only: it appears in responses once a server-recorded option was tapped
// (By "user") or the deadline sweep applied OnExpire (By "expired"; Option is
// then "none" when no default was set). Never send it - the server strips it
// from writes, and re-sending Options starts a new round and clears it.
// Unlike the other server-owned fields this one IS decoded here on purpose:
// polling an activity until Answer is set is how a producer with no webhook
// endpoint of its own reads the outcome.
type ApprovalAnswer struct {
	Option string `json:"option"`
	At     int64  `json:"at"`
	By     string `json:"by"`
}

// Content is the superset of all content fields used across integrations.
// Unused fields use omitempty and won't appear in JSON.
type Content struct {
	// Core fields (all templates)
	Template        string  `json:"template"`
	Progress        float64 `json:"progress"`
	State           string  `json:"state,omitempty"`
	Icon            string  `json:"icon,omitempty"`
	Subtitle        string  `json:"subtitle,omitempty"`
	AccentColor     string  `json:"accent_color,omitempty"`
	BackgroundColor string  `json:"background_color,omitempty"`
	TextColor       string  `json:"text_color,omitempty"`
	RemainingTime   *int    `json:"remaining_time,omitempty"`
	URL             string  `json:"url,omitempty"`
	SecondaryURL    string  `json:"secondary_url,omitempty"`

	// Tap-action routing (any template). tap_action overrides the widget-wide
	// tap target; url_action / secondary_url_action render as routed buttons
	// alongside (and taking precedence over) the legacy URL / SecondaryURL
	// string fields.
	TapAction          *TapAction `json:"tap_action,omitempty"`
	URLAction          *TapAction `json:"url_action,omitempty"`
	SecondaryURLAction *TapAction `json:"secondary_url_action,omitempty"`

	// Alert template
	Severity      string `json:"severity,omitempty"`
	FiredAt       *int64 `json:"fired_at,omitempty"`
	SeverityLabel string `json:"severity_label,omitempty"`

	// Steps template
	CurrentStep *int      `json:"current_step,omitempty"`
	TotalSteps  *int      `json:"total_steps,omitempty"`
	StepRows    []int     `json:"step_rows,omitempty"`
	StepLabels  []string  `json:"step_labels,omitempty"`
	StepWeights []float64 `json:"step_weights,omitempty"`
	StepColors  []string  `json:"step_colors,omitempty"`

	// LiveProgress opts the generic and steps templates into client-side
	// interpolation of the bar and ETA between pushes. Requires end_date.
	// Generic fills the whole bar by end_date; steps fills only the current
	// step, across start_date..end_date.
	LiveProgress *bool `json:"live_progress,omitempty"`

	// Activity image (generic, steps and media templates only - the server
	// answers 422 for any other template). ImageURL must be https with a host
	// and no userinfo, at most 2048 runes; the server never fetches it, the
	// device does, and the device additionally refuses private/LAN hosts, so a
	// LAN URL is accepted by the API but never renders. ImageThumbhash is a
	// padded standard-alphabet base64 thumbhash (at most 64 chars) rendered as
	// a blurred placeholder, and is the only tier that shows when the URL is
	// unreachable - for a LAN media server it IS the image. ImageShape defaults
	// to ImageShapeSquare when omitted. Switching to a template that has no
	// image slot clears all three server-side, so a merge-patch never has to
	// null them; a switch between generic, steps and media keeps them. Sending
	// one of the three on a template without a slot is a 422 either way.
	ImageURL       string     `json:"image_url,omitempty"`
	ImageShape     ImageShape `json:"image_shape,omitempty"`
	ImageThumbhash string     `json:"image_thumbhash,omitempty"`

	// Countdown template
	Duration          *string `json:"duration,omitempty"`
	EndDate           *int64  `json:"end_date,omitempty"`
	StartDate         *int64  `json:"start_date,omitempty"`
	WarningThreshold  *int    `json:"warning_threshold,omitempty"`
	CompletionMessage string  `json:"completion_message,omitempty"`
	// Alarm opts in to iOS 26 AlarmKit scheduling at end_date. Persists across
	// partial merge-patch updates until cleared by a transition to ENDED or by
	// patching content.alarm to explicit null. iOS 26+ only.
	Alarm *bool `json:"alarm,omitempty"`
	// SnoozeSeconds sets how far POST /activities/{slug}/snooze extends end_date
	// (and the iOS AlarmKit snooze window). 60-3600; server defaults to 300 when
	// omitted. Only meaningful with Alarm set.
	SnoozeSeconds *int `json:"snooze_seconds,omitempty"`

	// Gauge template: Value is float64
	// Timeline template: Value is map[string]float64
	Value    any      `json:"value,omitempty"`
	MinValue *float64 `json:"min_value,omitempty"`
	MaxValue *float64 `json:"max_value,omitempty"`
	Unit     string   `json:"unit,omitempty"`

	// Timeline template
	Scale      string                    `json:"scale,omitempty"`
	Decimals   *int                      `json:"decimals,omitempty"`
	Smoothing  *bool                     `json:"smoothing,omitempty"`
	Thresholds []Threshold               `json:"thresholds,omitempty"`
	Units      map[string]string         `json:"units,omitempty"`
	History    map[string][]HistoryPoint `json:"history,omitempty"`
	// PrimarySeries names the timeline series driving the headline number and
	// compact range. Falls back to the first series alphabetically.
	PrimarySeries string `json:"primary_series,omitempty"`

	// Board template: 1-4 tiles, replaced wholesale on each update.
	Tiles []BoardTile `json:"tiles,omitempty"`

	// Log template: 1-20 lines, newest-first, replaced wholesale on each
	// update. The server-accumulated log_backlog is read-only and omitted here
	// (this client never reads it).
	Lines []LogLine `json:"lines,omitempty"`

	// Media template (all optional). MediaTitle is the big line, at most 128
	// runes - the activity name is the source device ("Living Room") and
	// Subtitle carries the artist / show / channel. PlaybackState defaults to
	// PlaybackPaused. PositionSeconds (>= 0) is the playhead sampled at
	// PositionAt (unix seconds; the server stamps its own receipt time when
	// omitted and rejects a value more than 300s in the future), and the
	// device ticks the bar from there while playing. DurationSeconds (> 0, at
	// most 604800) omitted means indeterminate: no bar, elapsed still ticks.
	// Volume is 0..1 and draws a thin bar between the volume buttons; Favorite
	// fills the heart. Any of the eight on another template is a 422, and a
	// switch away from media clears them server-side. url_action /
	// secondary_url_action are accepted but not rendered on media; tap_action
	// keeps working.
	MediaTitle      string         `json:"media_title,omitempty"`
	PlaybackState   PlaybackState  `json:"playback_state,omitempty"`
	PositionSeconds *float64       `json:"position_seconds,omitempty"`
	DurationSeconds *float64       `json:"duration_seconds,omitempty"`
	PositionAt      *int64         `json:"position_at,omitempty"`
	Volume          *float64       `json:"volume,omitempty"`
	Favorite        *bool          `json:"favorite,omitempty"`
	Controls        *MediaControls `json:"controls,omitempty"`

	// Approval template: a question card (the question rides State) with 2-4
	// answer buttons. Options and Details replace wholesale on each update
	// (RFC 7396 array semantics), and re-sending Options starts a new round -
	// the server clears the stored Answer. Source (at most 24 runes) is the
	// producer badge in the card header. OnExpire names the option id the
	// server records when EndDate passes unanswered ("none" to just expire)
	// and requires EndDate. url_action / secondary_url_action are rejected on
	// this template (the server reserves those slots to keep older app builds
	// answerable), and so are alarm / snooze_seconds. Every field but Options
	// is optional; Answer is read-only, see its type.
	Options  []ApprovalOption `json:"options,omitempty"`
	Source   string           `json:"source,omitempty"`
	Details  []ApprovalDetail `json:"details,omitempty"`
	OnExpire string           `json:"on_expire,omitempty"`
	Answer   *ApprovalAnswer  `json:"answer,omitempty"`
}

// CreateActivityRequest is the body for POST /activities.
type CreateActivityRequest struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Priority int    `json:"priority"`
	EndedTTL int    `json:"ended_ttl,omitempty"`
	StaleTTL int    `json:"stale_ttl,omitempty"`
	// DismissalTTL is seconds after ENDED before the Live Activity leaves the
	// iOS Lock Screen: 0 removes it immediately, max 14400 (4h). Unset (nil)
	// keeps the server default (removal follows ended_ttl, capped at 4h). A
	// pointer, unlike the TTLs above, because 0 is meaningful and must not be
	// collapsed by omitempty.
	DismissalTTL *int `json:"dismissal_ttl,omitempty"`
}

// UpdateRequest is the body for the full-content PATCH /activities/{slug}
// used to seed a session or close it out with a final ENDED frame. For
// partial updates mid-session, prefer Client.PatchActivity with a
// ContentPatch.
//
// The three TTLs are top-level merge-patch fields on the server (omit = keep,
// null = clear, number = set). With omitempty a nil pointer means "keep" -
// this client cannot express the null-clear form, same as ContentPatch.
type UpdateRequest struct {
	State        string        `json:"state,omitempty"`
	Content      Content       `json:"content"`
	Sound        ActivitySound `json:"sound,omitempty"`
	Priority     *int          `json:"priority,omitempty"`
	EndedTTL     *int          `json:"ended_ttl,omitempty"`
	StaleTTL     *int          `json:"stale_ttl,omitempty"`
	DismissalTTL *int          `json:"dismissal_ttl,omitempty"`
}

// ContentPatch is the typed body for partial content updates. Unset pointer
// fields are omitted and preserved server-side under RFC 7396 merge-patch
// semantics. Use with Client.PatchActivity.
//
// Every pointer field MUST carry `json:",omitempty"` - without it a nil
// pointer marshals as JSON null, which per RFC 7396 section 2 instructs the server
// to delete the field. Adding a new pointer field without omitempty is a
// silent correctness bug.
type ContentPatch struct {
	Template        *string  `json:"template,omitempty"`
	Progress        *float64 `json:"progress,omitempty"`
	State           *string  `json:"state,omitempty"`
	Icon            *string  `json:"icon,omitempty"`
	Subtitle        *string  `json:"subtitle,omitempty"`
	AccentColor     *string  `json:"accent_color,omitempty"`
	BackgroundColor *string  `json:"background_color,omitempty"`
	TextColor       *string  `json:"text_color,omitempty"`
	RemainingTime   *int     `json:"remaining_time,omitempty"`
	URL             *string  `json:"url,omitempty"`
	SecondaryURL    *string  `json:"secondary_url,omitempty"`

	// Tap-action routing (any template). Each slot is a *TapAction: nil is
	// omitted (preserve server-side). A present value is deep-merged into the
	// stored action per RFC 7396 (the set fields overwrite, omitted fields are
	// preserved), so send the whole action to fully replace it.
	TapAction          *TapAction `json:"tap_action,omitempty"`
	URLAction          *TapAction `json:"url_action,omitempty"`
	SecondaryURLAction *TapAction `json:"secondary_url_action,omitempty"`

	// Alert template
	Severity      *string `json:"severity,omitempty"`
	FiredAt       *int64  `json:"fired_at,omitempty"`
	SeverityLabel *string `json:"severity_label,omitempty"`

	// Steps template
	CurrentStep *int      `json:"current_step,omitempty"`
	TotalSteps  *int      `json:"total_steps,omitempty"`
	StepRows    []int     `json:"step_rows,omitempty"`
	StepLabels  []string  `json:"step_labels,omitempty"`
	StepWeights []float64 `json:"step_weights,omitempty"`
	StepColors  []string  `json:"step_colors,omitempty"`

	LiveProgress *bool `json:"live_progress,omitempty"`

	// Activity image (generic, steps and media templates only). Same rules as
	// the matching Content fields; pointers here so an unset slot is omitted
	// and preserved server-side rather than deleted. Switching to a template
	// with no image slot clears all three server-side on its own; a switch
	// between generic, steps and media keeps them.
	ImageURL       *string     `json:"image_url,omitempty"`
	ImageShape     *ImageShape `json:"image_shape,omitempty"`
	ImageThumbhash *string     `json:"image_thumbhash,omitempty"`

	// Countdown template
	Duration          *string `json:"duration,omitempty"`
	EndDate           *int64  `json:"end_date,omitempty"`
	StartDate         *int64  `json:"start_date,omitempty"`
	WarningThreshold  *int    `json:"warning_threshold,omitempty"`
	CompletionMessage *string `json:"completion_message,omitempty"`
	Alarm             *bool   `json:"alarm,omitempty"`
	SnoozeSeconds     *int    `json:"snooze_seconds,omitempty"`

	// Gauge template: Value is float64
	// Timeline template: Value is map[string]float64
	Value    any      `json:"value,omitempty"`
	MinValue *float64 `json:"min_value,omitempty"`
	MaxValue *float64 `json:"max_value,omitempty"`
	Unit     *string  `json:"unit,omitempty"`

	// Timeline template
	Scale      *string                   `json:"scale,omitempty"`
	Decimals   *int                      `json:"decimals,omitempty"`
	Smoothing  *bool                     `json:"smoothing,omitempty"`
	Thresholds []Threshold               `json:"thresholds,omitempty"`
	Units      map[string]string         `json:"units,omitempty"`
	History    map[string][]HistoryPoint `json:"history,omitempty"`
	// PrimarySeries names the timeline series driving the headline number and
	// compact range. Falls back to the first series alphabetically.
	PrimarySeries *string `json:"primary_series,omitempty"`

	// Board template: 1-4 tiles. Sending the slice replaces all tiles
	// (RFC 7396 array semantics); omitting it preserves the stored tiles.
	Tiles []BoardTile `json:"tiles,omitempty"`

	// Log template: 1-20 lines, newest-first. Sending the slice replaces the
	// live line snapshot; omitting it preserves the stored lines.
	Lines []LogLine `json:"lines,omitempty"`

	// Media template. Same rules as the matching Content fields. A patch that
	// re-sends PositionSeconds without PositionAt makes the server drop the
	// stored PositionAt and stamp its receipt time, so a position tick never
	// inherits a stale sample time; send both to keep your own clock. Controls
	// deep-merges per RFC 7396 (a slot you send overwrites that slot, omitted
	// slots are preserved; clearing one slot needs an explicit null, which nil
	// pointers cannot express here), while Controls.Extra replaces wholesale.
	// Ticks that carry only position_seconds / position_at go out as
	// low-priority coalescable pushes; every other media field is structural.
	MediaTitle      *string        `json:"media_title,omitempty"`
	PlaybackState   *PlaybackState `json:"playback_state,omitempty"`
	PositionSeconds *float64       `json:"position_seconds,omitempty"`
	DurationSeconds *float64       `json:"duration_seconds,omitempty"`
	PositionAt      *int64         `json:"position_at,omitempty"`
	Volume          *float64       `json:"volume,omitempty"`
	Favorite        *bool          `json:"favorite,omitempty"`
	Controls        *MediaControls `json:"controls,omitempty"`

	// Approval template. Options and Details replace wholesale (RFC 7396
	// array semantics); re-sending Options starts a new round and clears the
	// server-recorded answer. The answer itself is server-owned and has no
	// patch field - read it back from the response Content.
	Options  []ApprovalOption `json:"options,omitempty"`
	Source   *string          `json:"source,omitempty"`
	Details  []ApprovalDetail `json:"details,omitempty"`
	OnExpire *string          `json:"on_expire,omitempty"`
}

// PatchRequest is the typed body for PATCH /activities/{slug}. State is a
// plain string with omitempty so that tick updates can leave it unset and
// rely on the server preserving the stored state.
type PatchRequest struct {
	State    string        `json:"state,omitempty"`
	Content  *ContentPatch `json:"content,omitempty"`
	Sound    ActivitySound `json:"sound,omitempty"`
	Priority *int          `json:"priority,omitempty"`

	// The three TTLs are top-level merge-patch fields on the server (omitted
	// keeps, null clears, a number sets). With omitempty a nil pointer means
	// "keep"; the null-clear form is unreachable from this client, the same
	// limitation UpdateRequest and ContentPatch carry. No caller wants it.
	EndedTTL     *int `json:"ended_ttl,omitempty"`
	StaleTTL     *int `json:"stale_ttl,omitempty"`
	DismissalTTL *int `json:"dismissal_ttl,omitempty"`
}

// WidgetTemplate names a renderer on the iOS widget extension.
type WidgetTemplate string

const (
	WidgetTemplateValue     WidgetTemplate = "value"
	WidgetTemplateProgress  WidgetTemplate = "progress"
	WidgetTemplateStatus    WidgetTemplate = "status"
	WidgetTemplateGauge     WidgetTemplate = "gauge"
	WidgetTemplateStatList  WidgetTemplate = "stat_list"
	WidgetTemplateTrend     WidgetTemplate = "trend"
	WidgetTemplateCountdown WidgetTemplate = "countdown"
	WidgetTemplateBattery   WidgetTemplate = "battery"
	WidgetTemplateSchedule  WidgetTemplate = "schedule"
	WidgetTemplateFlow      WidgetTemplate = "flow"
)

// Timer style constants pick how a TimerValue renders. Mirrors the server's
// timer style enum; an empty style behaves as TimerStyleTimer.
const (
	TimerStyleTimer    = "timer"
	TimerStyleRelative = "relative"
)

// Schedule level constants band one period of a schedule widget. Mirrors the
// server's schedule level enum; empty leaves the banding to the client, which
// derives it from the posted range.
const (
	ScheduleLevelLow    = "low"
	ScheduleLevelMedium = "medium"
	ScheduleLevelHigh   = "high"
)

// TimerValue turns a text slot into a self-updating timer rendered on device.
// A past Date counts up, a future one counts down, and WidgetKit re-renders the
// text itself - no repeat pushes. Style is TimerStyleTimer (default, ticks like
// 01:23:45) or TimerStyleRelative (coarse units, "2 min").
type TimerValue struct {
	Date  time.Time `json:"date"`
	Style string    `json:"style,omitempty"`
}

// StatRow is a single row of a stat_list widget. Value is pre-formatted by
// the integration (server does not localize / round); Unit is optional and
// rendered after the value.
type StatRow struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Unit  string `json:"unit,omitempty"`
	// Timer renders the row's trailing text as a live timer where the client
	// supports it. Value stays required as the fallback on older builds.
	Timer *TimerValue `json:"timer,omitempty"`
}

// BatteryDevice is one ring of a battery widget (1-8 per widget), rendered in
// the Apple Batteries idiom. Level is the required 0-100 charge percentage; it
// deliberately carries no omitempty, mirroring the server, so an unset level
// arrives as an explicit null and is rejected instead of vanishing from the
// payload and being read as a preserved older value.
type BatteryDevice struct {
	Name     string   `json:"name"`
	Level    *float64 `json:"level"`
	Charging bool     `json:"charging,omitempty"`
	Icon     string   `json:"icon,omitempty"`
	Color    string   `json:"color,omitempty"`
}

// DeviceSortKey is one ordering key the server applies to a battery widget's
// BatteryDevices before storing them. At most two keys, applied in order, so a
// second key only breaks a tie the first left. Field is one of
// DeviceSortFieldLevel/Name, Direction one of DeviceSortAsc/Desc; an empty
// direction is read as ascending.
type DeviceSortKey struct {
	Field     string `json:"field"`
	Direction string `json:"direction,omitempty"`
}

// Allowed DeviceSortKey field and direction values (mirror pushward-server's
// validDeviceSortFields / validDeviceSortDirections).
const (
	DeviceSortFieldLevel = "level"
	DeviceSortFieldName  = "name"

	DeviceSortAsc  = "asc"
	DeviceSortDesc = "desc"
)

// SchedulePeriod is one period of a schedule widget timeline - an hourly energy
// tariff, a delivery window, a shift. 1-48 periods per widget, strictly
// increasing by Start; each period runs until the next one starts and the
// client highlights the one containing now. Value is unit-agnostic, labelled by
// the content-level Unit. Level is one of ScheduleLevelLow/Medium/High.
type SchedulePeriod struct {
	Start time.Time `json:"start"`
	Value *float64  `json:"value"`
	Level string    `json:"level,omitempty"`
}

// FlowNode is one endpoint of a flow widget. The template is domain-agnostic:
// energy is the motivating case (solar in, grid exchange, battery storage,
// house draw), but water, data or money fit the same slots. Sign convention
// depends on the slot - exchange Rate is positive inbound and negative
// outbound, storage Rate is positive while filling and negative while
// draining. Level is the 0-100 fill and only means anything on storage.
type FlowNode struct {
	Name  string   `json:"name,omitempty"`
	Rate  *float64 `json:"rate"`
	Total *float64 `json:"total,omitempty"`
	Level *float64 `json:"level,omitempty"`
	Icon  string   `json:"icon,omitempty"`
	Color string   `json:"color,omitempty"`
}

// WidgetFlow groups a flow widget's nodes into slots: what comes in, what
// buffers it, what is traded with the outside, and what consumes it. At least
// one slot must be set; Inputs holds at most 3.
type WidgetFlow struct {
	Inputs   []FlowNode `json:"inputs,omitempty"`
	Output   *FlowNode  `json:"output,omitempty"`
	Storage  *FlowNode  `json:"storage,omitempty"`
	Exchange *FlowNode  `json:"exchange,omitempty"`
}

// WidgetContent mirrors the server's widget content model. All fields are
// optional and respect RFC 7396 merge-patch semantics when sent via
// Client.UpdateWidget: pointer fields omitted with nil + omitempty are
// preserved, present pointer fields overwrite, and explicit JSON null on a
// pointer (achieved only by removing omitempty) clears the field server-side.
type WidgetContent struct {
	Template        WidgetTemplate `json:"template,omitempty"`
	Icon            string         `json:"icon,omitempty"`
	Value           *float64       `json:"value,omitempty"`
	MinValue        *float64       `json:"min_value,omitempty"`
	MaxValue        *float64       `json:"max_value,omitempty"`
	Unit            string         `json:"unit,omitempty"`
	Label           string         `json:"label,omitempty"`
	Subtitle        string         `json:"subtitle,omitempty"`
	Severity        string         `json:"severity,omitempty"`
	AccentColor     string         `json:"accent_color,omitempty"`
	BackgroundColor string         `json:"background_color,omitempty"`
	TextColor       string         `json:"text_color,omitempty"`
	// Trend annotates value/gauge widgets with a directional arrow. One of
	// "up" / "down" / "flat". Ignored for other templates.
	Trend string `json:"trend,omitempty"`
	// StatRows powers the stat_list template - a 1-6 row label/value list.
	// Required when template == stat_list, ignored otherwise.
	StatRows []StatRow `json:"stat_rows,omitempty"`
	// Points powers the trend template - 2-48 sparkline samples, oldest first,
	// alongside a required Value. Chart bounds come from MinValue/MaxValue when
	// set, otherwise the client auto-scales.
	Points []float64 `json:"points,omitempty"`
	// StartDate / EndDate drive the countdown template (EndDate required) and
	// self-advancing progress: a progress widget carrying both dates advances
	// its bar on device without further pushes.
	StartDate *time.Time `json:"start_date,omitempty"`
	EndDate   *time.Time `json:"end_date,omitempty"`
	// ExpiredText replaces the countdown once EndDate passes. Without it the
	// client counts up from EndDate instead.
	ExpiredText string `json:"expired_text,omitempty"`
	// BatteryDevices powers the battery template - 1-8 device rings. Required
	// when template == battery, ignored otherwise.
	BatteryDevices []BatteryDevice `json:"devices,omitempty"`
	// DeviceSort reorders BatteryDevices server-side before they are stored, so
	// the prefix each widget family renders (2 small, 4 medium, 8 large) holds
	// the devices that matter. Empty keeps the order as sent. Applies to the
	// battery template, ignored for the others.
	DeviceSort []DeviceSortKey `json:"device_sort,omitempty"`
	// Periods powers the schedule template - 1-48 periods in strictly
	// increasing start order. Required when template == schedule.
	Periods []SchedulePeriod `json:"periods,omitempty"`
	// Flow powers the flow template. Required when template == flow.
	Flow *WidgetFlow `json:"flow,omitempty"`
	// SubtitleTimer renders the subtitle slot as a live timer on any template;
	// the static Subtitle is what older clients fall back to.
	SubtitleTimer *TimerValue `json:"subtitle_timer,omitempty"`
	// Tap-action routing on a widget. tap_action overrides the whole-widget tap
	// target; url_action / secondary_url_action render as routed buttons. Mirrors
	// the same slots on activity Content.
	TapAction          *TapAction `json:"tap_action,omitempty"`
	URLAction          *TapAction `json:"url_action,omitempty"`
	SecondaryURLAction *TapAction `json:"secondary_url_action,omitempty"`
}

// CreateWidgetRequest is the body for POST /widgets. The server upserts on
// (user, slug); a duplicate slug is not an error.
type CreateWidgetRequest struct {
	Slug         string        `json:"slug"`
	Name         string        `json:"name"`
	Content      WidgetContent `json:"content"`
	PushThrottle *int          `json:"push_throttle,omitempty"`
	// StaleAfter is how many seconds after the last update clients start
	// rendering the widget as stale (60-604800). Nil means never demoted, so
	// set it a little above the polling interval on anything that can go quiet.
	StaleAfter *int `json:"stale_after,omitempty"`
}

// UpdateWidgetRequest is the body for PATCH /widgets/{slug}. The server
// requires Content-Type "application/merge-patch+json" and applies RFC 7396
// merge semantics: present top-level fields overwrite; absent fields are
// preserved.
//
// Content is a pointer so that callers who only want to patch Name or
// PushThrottle leave it nil and the field is omitted from the wire payload
// entirely. Sending `"content":{}` would otherwise round-trip an empty
// struct through the server's struct-typed handler and risk clearing
// existing content fields.
type UpdateWidgetRequest struct {
	Name         string         `json:"name,omitempty"`
	Content      *WidgetContent `json:"content,omitempty"`
	PushThrottle *int           `json:"push_throttle,omitempty"`
	// StaleAfter re-tunes the staleness window (60-604800). Nil is omitted and
	// preserves the stored value; as with the other pointers here, omitempty
	// means this client cannot express the null-clear form.
	StaleAfter *int `json:"stale_after,omitempty"`
}

// MediaAttachment is a rich media attachment (image, video, or audio)
// attached to a notification. The iOS client downloads the URL and
// attaches it via UNNotificationAttachment subject to Apple's per-type
// size caps (image 10 MB, audio 5 MB, video 50 MB). HTTPS only.
type MediaAttachment struct {
	URL  string `json:"url"`
	Type string `json:"type"` // "image" | "video" | "audio"
}

// MediaImage returns a MediaAttachment for an image URL, or nil if the
// URL is empty. Convenience for integrations that forward poster art,
// monitoring screenshots, or other image media without typing the full
// literal at every call site.
func MediaImage(url string) *MediaAttachment {
	if url == "" {
		return nil
	}
	return &MediaAttachment{URL: url, Type: "image"}
}

// NotificationAction is one server-driven action button shown on a push
// notification. Tapping the button surfaces the action's `id` to the iOS
// app, which routes by ID and opens the action's `url` if set.
//
// Method, Headers and Body turn the button into a silent webhook, using the
// same routing rules the iOS dispatcher applies to TapAction:
//   - custom scheme (e.g. homeassistant://) -> opens that app; Method/Headers/
//     Body are ignored
//   - http(s) + Foreground=true -> opens the URL in Safari / the in-app browser
//   - http(s) + Foreground=false -> fires Method/Headers/Body silently
//
// Headers must total 1KB or less and Body 1024 chars or less; the server
// rejects Method/Headers/Body on custom-scheme URLs.
//
// TextInput turns the button into a reply-with-text action: tapping it shows an
// inline text field, and the typed text replaces {{input}} in Body (or is sent
// as {"text": ...} when Body has no placeholder). It requires a silent
// (Foreground=false) http(s) action; the server rejects it otherwise.
type NotificationAction struct {
	ID                     string            `json:"id"`
	Title                  string            `json:"title"`
	URL                    string            `json:"url,omitempty"`
	Foreground             bool              `json:"foreground,omitempty"`
	Method                 string            `json:"method,omitempty"` // GET, POST, PUT, PATCH, DELETE, HEAD
	Headers                map[string]string `json:"headers,omitempty"`
	Body                   string            `json:"body,omitempty"`
	Destructive            bool              `json:"destructive,omitempty"`
	AuthenticationRequired bool              `json:"authentication_required,omitempty"`
	Icon                   string            `json:"icon,omitempty"` // SF Symbol name
	TextInput              bool              `json:"text_input,omitempty"`
	TextInputPlaceholder   string            `json:"text_input_placeholder,omitempty"`
	TextInputButtonTitle   string            `json:"text_input_button_title,omitempty"`
}

// SendNotificationRequest is the body for POST /notifications.
type SendNotificationRequest struct {
	Title             string               `json:"title"`
	Subtitle          string               `json:"subtitle,omitempty"`
	Body              string               `json:"body"`
	ThreadID          string               `json:"thread_id,omitempty"`
	CollapseID        string               `json:"collapse_id,omitempty"`
	Level             string               `json:"level,omitempty"`
	Volume            *float64             `json:"volume,omitempty"` // Sound volume for critical alerts (0.0-1.0)
	Source            string               `json:"source,omitempty"`
	SourceDisplayName string               `json:"source_display_name,omitempty"`
	URL               string               `json:"url,omitempty"`
	Media             *MediaAttachment     `json:"media,omitempty"`
	IconURL           string               `json:"icon_url,omitempty"`
	Metadata          map[string]string    `json:"metadata,omitempty"`
	Actions           []NotificationAction `json:"actions,omitempty"`
	// ActivitySlug links the notification to an existing activity, so tapping
	// it deep-links into that Live Activity. A slug the caller does not own is
	// rejected with 422 notification.activity_not_found before the
	// notification is persisted.
	ActivitySlug string `json:"activity_slug,omitempty"`
	// Push controls APNs delivery. Nil omits the key and the server applies
	// its default of true; BoolPtr(false) stores the notification in the inbox
	// without alerting any device. It is a pointer so that an unset field
	// cannot silently mean "inbox only".
	Push *bool `json:"push,omitempty"`
}

// sourceDisplayNames maps source identifiers to their human-readable display names.
var sourceDisplayNames = map[string]string{
	"grafana":         "Grafana",
	"argocd":          "ArgoCD",
	"radarr":          "Radarr",
	"sonarr":          "Sonarr",
	"prowlarr":        "Prowlarr",
	"jellyfin":        "Jellyfin",
	"paperless":       "Paperless-ngx",
	"changedetection": "Changedetection.io",
	"unmanic":         "Unmanic",
	"bazarr":          "Bazarr",
	"proxmox":         "Proxmox",
	"overseerr":       "Overseerr",
	"uptimekuma":      "Uptime Kuma",
	"gatus":           "Gatus",
	"backrest":        "Backrest",
	"komodo":          "Komodo",
	"truenas":         "TrueNAS",
	"sabnzbd":         "SABnzbd",
	"github":          "GitHub",
	"forgejo":         "Forgejo",
	"bambulab":        "BambuLab",
	"octoprint":       "OctoPrint",
	"mqtt":            "MQTT",
}

// DisplayNameFor returns the display name for a source, falling back to the identifier itself.
func DisplayNameFor(source string) string {
	if name, ok := sourceDisplayNames[source]; ok {
		return name
	}
	return source
}

// FillSourceDisplayName sets SourceDisplayName from Source when not already set.
func (r *SendNotificationRequest) FillSourceDisplayName() {
	if r.SourceDisplayName == "" && r.Source != "" {
		r.SourceDisplayName = DisplayNameFor(r.Source)
	}
}
