package widgets

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

func TestStatRowsEqual(t *testing.T) {
	a := []pushward.StatRow{{Label: "x", Value: "1"}, {Label: "y", Value: "2"}}
	b := []pushward.StatRow{{Label: "x", Value: "1"}, {Label: "y", Value: "2"}}
	if !statRowsEqual(a, b) {
		t.Error("identical slices should be equal")
	}
	if !statRowsEqual(nil, nil) {
		t.Error("two nil slices should be equal")
	}
	if statRowsEqual(a, b[:1]) {
		t.Error("different lengths should not be equal")
	}
	c := []pushward.StatRow{{Label: "x", Value: "1"}, {Label: "y", Value: "3"}}
	if statRowsEqual(a, c) {
		t.Error("different values should not be equal")
	}
}

func TestTrimStatRows(t *testing.T) {
	in := []pushward.StatRow{{Label: "a"}, {Label: "b"}, {Label: "c"}, {Label: "d"}, {Label: "e"}}
	got := trimStatRows(in, 3)
	if len(got) != 3 {
		t.Errorf("len = %d, want 3", len(got))
	}
	if trimStatRows(in, 0) == nil {
		t.Error("trim with 0 should fall back to DefaultMaxStatRows, not nil")
	}
	if len(trimStatRows(in, 0)) != DefaultMaxStatRows {
		t.Errorf("default cap = %d, want %d", len(trimStatRows(in, 0)), DefaultMaxStatRows)
	}
	short := []pushward.StatRow{{Label: "only"}}
	if len(trimStatRows(short, 3)) != 1 {
		t.Error("trim should leave shorter slices unchanged")
	}
}

func TestManager_StatList_CreatesWidgetWithRows(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := StatListSourceFunc(func(_ context.Context) ([]pushward.StatRow, error) {
		return []pushward.StatRow{
			{Label: "Users", Value: "42"},
			{Label: "MRR", Value: "$8 333", Unit: "USD"},
		}, nil
	})

	m, err := New(client, []Spec{{
		Slug:           "homelab",
		Name:           "Homelab",
		StatListSource: src,
		Interval:       20 * time.Millisecond,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	m.Wait()

	if stub.creates.Load() != 1 {
		t.Fatalf("creates = %d, want 1", stub.creates.Load())
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	got := stub.gotCreate[0]
	if got.Template != pushward.WidgetTemplateStatList {
		t.Errorf("Template = %q, want stat_list (defaulted)", got.Template)
	}
	if len(got.Content.StatRows) != 2 || got.Content.StatRows[0].Label != "Users" {
		t.Errorf("StatRows mismatch: %+v", got.Content.StatRows)
	}
}

func TestManager_StatList_SkipsPatchOnSameRows(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := StatListSourceFunc(func(_ context.Context) ([]pushward.StatRow, error) {
		return []pushward.StatRow{{Label: "Users", Value: "42"}}, nil
	})

	m, err := New(client, []Spec{{
		Slug:           "homelab",
		Name:           "Homelab",
		StatListSource: src,
		Interval:       15 * time.Millisecond,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = m.Start(ctx)
	time.Sleep(120 * time.Millisecond)
	cancel()
	m.Wait()

	if stub.updates.Load() != 0 {
		t.Errorf("expected 0 PATCH for unchanged rows, got %d", stub.updates.Load())
	}
}

func TestManager_StatList_PatchesWhenRowChanges(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	var i atomic.Int64
	src := StatListSourceFunc(func(_ context.Context) ([]pushward.StatRow, error) {
		n := i.Add(1)
		return []pushward.StatRow{{Label: "n", Value: stringOf(n)}}, nil
	})

	m, err := New(client, []Spec{{
		Slug:           "ticker",
		StatListSource: src,
		Interval:       15 * time.Millisecond,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = m.Start(ctx)
	waitFor(t, 500*time.Millisecond, func() bool { return stub.updates.Load() >= 2 })
	cancel()
	m.Wait()
}

func TestManager_StatList_TrimsToCap(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := StatListSourceFunc(func(_ context.Context) ([]pushward.StatRow, error) {
		return []pushward.StatRow{
			{Label: "1", Value: "1"}, {Label: "2", Value: "2"},
			{Label: "3", Value: "3"}, {Label: "4", Value: "4"},
			{Label: "5", Value: "5"}, {Label: "6", Value: "6"},
		}, nil
	})

	m, err := New(client, []Spec{{
		Slug:           "capped",
		StatListSource: src,
		Interval:       30 * time.Millisecond,
		MaxStatRows:    3,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = m.Start(ctx)
	cancel()
	m.Wait()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if got := len(stub.gotCreate[0].Content.StatRows); got != 3 {
		t.Errorf("rows = %d, want 3 (capped)", got)
	}
}

// stringOf renders an int64 without pulling in strconv at every call site —
// keeps test files compact.
func stringOf(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}
