package pushward

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestClampStepShape pins the bounds that froze pushward-grafana-plugin's card in
// production: "e2e test grafana-enterprise@nightly" is 35 runes against a bound of
// 32, and the server rejects the WHOLE payload rather than the offending entry.
func TestClampStepShape(t *testing.T) {
	rows, labels := ClampStepShape(
		[]int{0, 1, 11, 10},
		[]string{"Build", "e2e test grafana-enterprise@nightly", "", "Unit Tests"},
	)

	if want := []int{MinStepRows, 1, MaxStepRows, 10}; !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
	wantLabels := []string{"Build", "e2e test grafana-enterprise@nigh", "", "Unit Tests"}
	if !reflect.DeepEqual(labels, wantLabels) {
		t.Errorf("labels = %q, want %q", labels, wantLabels)
	}
	for i, l := range labels {
		if n := utf8.RuneCountInString(l); n > MaxStepLabelLen {
			t.Errorf("labels[%d] is %d runes, over the bound of %d", i, n, MaxStepLabelLen)
		}
	}
}

// TestClampStepShapeBoundary covers the off-by-one: the bound is inclusive, so a
// label of exactly MaxStepLabelLen must come back whole. Truncating it would
// corrupt text the server would have accepted.
func TestClampStepShapeBoundary(t *testing.T) {
	exact := strings.Repeat("e", MaxStepLabelLen)
	_, got := ClampStepShape([]int{1}, []string{exact})
	if got[0] != exact {
		t.Errorf("a %d-rune label was truncated to %q", MaxStepLabelLen, got[0])
	}
}

// TestClampStepShapeKeepsGroupsDistinct pins that the clamp is cosmetic. Two
// labels differing only past the bound read alike afterwards, but folding them
// into one entry would under-count total_steps and mis-attribute progress.
func TestClampStepShapeKeepsGroupsDistinct(t *testing.T) {
	_, pair := ClampStepShape([]int{1, 1}, []string{
		"e2e test grafana-enterprise@nightly-a", "e2e test grafana-enterprise@nightly-b",
	})
	if len(pair) != 2 {
		t.Fatalf("clamping changed the step count: %q", pair)
	}
	if pair[0] != pair[1] {
		t.Errorf("expected both to clamp to the same prefix, got %q", pair)
	}
}

// TestClampStepShapeEmpty pins that an empty shape stays empty rather than
// growing entries the caller never had; omitempty then drops the field.
func TestClampStepShapeEmpty(t *testing.T) {
	rows, labels := ClampStepShape(nil, nil)
	if len(rows) != 0 || len(labels) != 0 {
		t.Errorf("ClampStepShape(nil, nil) = %v, %q, want empty", rows, labels)
	}
}

// TestMediaWireNames pins the snake_case keys of the media template on both
// Content and ContentPatch, and that an empty patch leaks none of them: a nil
// pointer without omitempty would marshal as null and delete the stored value
// under merge-patch.
func TestMediaWireNames(t *testing.T) {
	controls := &MediaControls{
		Previous:   &TapAction{URL: "https://ha.example/prev"},
		PlayPause:  &TapAction{URL: "https://ha.example/toggle"},
		Play:       &TapAction{URL: "https://ha.example/play"},
		Pause:      &TapAction{URL: "https://ha.example/pause"},
		Next:       &TapAction{URL: "https://ha.example/next"},
		Stop:       &TapAction{URL: "https://ha.example/stop"},
		Favorite:   &TapAction{URL: "https://ha.example/fav"},
		VolumeDown: &TapAction{URL: "https://ha.example/vdown"},
		VolumeUp:   &TapAction{URL: "https://ha.example/vup"},
		Extra:      []TapAction{{URL: "spotify://queue", Icon: "list.bullet"}},
	}
	wantMedia := []string{
		"controls", "duration_seconds", "favorite", "media_title", "playback_state",
		"position_at", "position_seconds", "volume",
	}
	wantControls := []string{
		"extra", "favorite", "next", "pause", "play", "play_pause", "previous", "stop",
		"volume_down", "volume_up",
	}

	full := Content{
		Template:        TemplateMedia,
		MediaTitle:      "Snooze",
		PlaybackState:   PlaybackPlaying,
		PositionSeconds: Float64Ptr(47.5),
		DurationSeconds: Float64Ptr(214),
		PositionAt:      Int64Ptr(1755500000),
		Volume:          Float64Ptr(0.35),
		Favorite:        BoolPtr(true),
		Controls:        controls,
	}
	patch := ContentPatch{
		MediaTitle:      StringPtr("Snooze"),
		PlaybackState:   func() *PlaybackState { s := PlaybackPaused; return &s }(),
		PositionSeconds: Float64Ptr(47.5),
		DurationSeconds: Float64Ptr(214),
		PositionAt:      Int64Ptr(1755500000),
		Volume:          Float64Ptr(0.35),
		Favorite:        BoolPtr(true),
		Controls:        controls,
	}
	for name, v := range map[string]any{"Content": full, "ContentPatch": patch} {
		got := jsonKeys(t, v)
		for _, k := range wantMedia {
			if !got[k] {
				t.Errorf("%s: media key %q missing from %v", name, k, sortedKeys(got))
			}
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(mustJSON(t, v), &raw); err != nil {
			t.Fatal(err)
		}
		var ctrl map[string]json.RawMessage
		if err := json.Unmarshal(raw["controls"], &ctrl); err != nil {
			t.Fatalf("%s: controls: %v", name, err)
		}
		if got := sortedKeys(setOf(ctrl)); !reflect.DeepEqual(got, wantControls) {
			t.Errorf("%s: controls keys = %v, want %v", name, got, wantControls)
		}
	}

	empty := jsonKeys(t, ContentPatch{})
	for _, k := range wantMedia {
		if empty[k] {
			t.Errorf("empty ContentPatch leaks %q (missing omitempty)", k)
		}
	}
	// A partly filled controls object omits its unset slots too, so a patch
	// touching one button preserves the others server-side.
	one := jsonKeys(t, MediaControls{Stop: &TapAction{URL: "https://ha.example/stop"}})
	if got := sortedKeys(one); !reflect.DeepEqual(got, []string{"stop"}) {
		t.Errorf("MediaControls{Stop} keys = %v, want [stop]", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func jsonKeys(t *testing.T, v any) map[string]bool {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(mustJSON(t, v), &raw); err != nil {
		t.Fatal(err)
	}
	return setOf(raw)
}

func setOf(m map[string]json.RawMessage) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
