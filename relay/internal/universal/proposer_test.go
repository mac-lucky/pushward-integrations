package universal

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type stubProposer struct {
	res   Result
	err   error
	sleep time.Duration
}

func (s stubProposer) Propose(context.Context, Input) (Result, error) {
	time.Sleep(s.sleep)
	return s.res, s.err
}

type panicProposer struct{}

func (*panicProposer) Propose(context.Context, Input) (Result, error) {
	panic("ranker bug")
}

func TestHeuristicProposer(t *testing.T) {
	fields, _ := flatten(t, `{"title": "Backup done", "status": "finished", "job_id": "a1b2c3"}`)
	res, err := Heuristic{}.Propose(context.Background(), Input{Source: "x", Fields: fields})
	if err != nil {
		t.Fatal(err)
	}
	if res.By != "heuristic/1" || res.Scores != nil || !reflect.DeepEqual(res.Proposal, Propose(fields)) {
		t.Errorf("Heuristic = %+v", res)
	}
}

func TestFallback(t *testing.T) {
	fields, _ := flatten(t, `{"title": "Backup done", "message": "All 3 snapshots written", "api_token": "abc"}`)
	in := Input{Source: "x", Fields: fields}
	heuristic := Propose(fields)
	good := Result{Proposal: Proposal{Title: "message", Body: "title", Kind: KindNotification}, By: "ranker/1"}

	cases := []struct {
		name    string
		primary Proposer
		timeout time.Duration
		reason  string
		want    Proposal
	}{
		{"primary wins", stubProposer{res: good}, time.Second, "", good.Proposal},
		{"no timeout set", stubProposer{res: good}, 0, "", good.Proposal},
		{"nil primary", nil, time.Second, "", heuristic},
		{"typed nil primary", (*panicProposer)(nil), time.Second, "", heuristic},
		{"error", stubProposer{err: errors.New("boom")}, time.Second, "error", heuristic},
		{"deadline error", stubProposer{err: context.DeadlineExceeded}, time.Second, "timeout", heuristic},
		{"panic", &panicProposer{}, time.Second, "panic", heuristic},
		{"panic, no timeout", &panicProposer{}, 0, "panic", heuristic},
		{"slow", stubProposer{res: good, sleep: 500 * time.Millisecond}, 20 * time.Millisecond, "timeout", heuristic},
		{"secret title", stubProposer{res: Result{Proposal: Proposal{Title: "api_token", Kind: KindNotification}}}, time.Second, "invalid", heuristic},
		{"missing path", stubProposer{res: Result{Proposal: Proposal{Title: "subject", Kind: KindNotification}}}, time.Second, "invalid", heuristic},
		{"no kind", stubProposer{res: Result{Proposal: Proposal{Title: "title"}}}, time.Second, "invalid", heuristic},
	}
	for _, c := range cases {
		var reasons []string
		f := &Fallback{Primary: c.primary, Timeout: c.timeout, OnFallback: func(r string) { reasons = append(reasons, r) }}
		start := time.Now()
		res, err := f.Propose(context.Background(), in)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !reflect.DeepEqual(res.Proposal, c.want) {
			t.Errorf("%s: proposal %+v, want %+v", c.name, res.Proposal, c.want)
		}
		var want []string
		if c.reason != "" {
			want = []string{c.reason}
		}
		if !reflect.DeepEqual(reasons, want) {
			t.Errorf("%s: OnFallback got %v, want %v", c.name, reasons, want)
		}
		if c.reason == "timeout" && time.Since(start) > 300*time.Millisecond {
			t.Errorf("%s: waited %v for a primary past its timeout", c.name, time.Since(start))
		}
	}
}

func TestFallbackCanceled(t *testing.T) {
	fields, _ := flatten(t, `{"title": "x"}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var reason string
	f := &Fallback{
		Primary:    stubProposer{sleep: 200 * time.Millisecond},
		Timeout:    time.Second,
		OnFallback: func(r string) { reason = r },
	}
	if _, err := f.Propose(ctx, Input{Fields: fields}); err != nil {
		t.Fatal(err)
	}
	if reason != "canceled" {
		t.Errorf("reason = %q, want canceled", reason)
	}
}

// Abandoned primaries hold their slot until they return; past maxPrimaries
// the Fallback stops starting new ones.
func TestFallbackBusy(t *testing.T) {
	fields, _ := flatten(t, `{"title": "x"}`)
	var reasons []string
	f := &Fallback{
		Primary:    stubProposer{sleep: 500 * time.Millisecond},
		Timeout:    5 * time.Millisecond,
		OnFallback: func(r string) { reasons = append(reasons, r) },
	}
	for range maxPrimaries + 1 {
		if _, err := f.Propose(context.Background(), Input{Fields: fields}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"timeout", "timeout", "timeout", "timeout", "busy"}
	if !reflect.DeepEqual(reasons, want) {
		t.Errorf("reasons = %v, want %v", reasons, want)
	}
}

func TestFallbackSecondary(t *testing.T) {
	fields, _ := flatten(t, `{"title": "x"}`)
	second := stubProposer{res: Result{Proposal: Proposal{Kind: KindAlert}, By: "second"}}
	f := &Fallback{Primary: stubProposer{err: errors.New("boom")}, Secondary: second}
	res, _ := f.Propose(context.Background(), Input{Fields: fields})
	if res.By != "second" {
		t.Errorf("By = %q, want the secondary's result", res.By)
	}
}
