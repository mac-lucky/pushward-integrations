package pushward

import (
	"reflect"
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
