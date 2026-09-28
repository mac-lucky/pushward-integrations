package universal

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"
)

// Input is one payload to propose a mapping for.
type Input struct {
	Source    string
	Fields    []Field
	Truncated bool

	shapes []ShapeField
}

// NewInput returns an Input whose shapes are already known, so no proposer
// computes them again. shapes must be ShapesOf(fields).
func NewInput(source string, fields []Field, shapes []ShapeField, truncated bool) Input {
	return Input{Source: source, Fields: fields, Truncated: truncated, shapes: shapes}
}

// Shapes returns ShapesOf(in.Fields). Fallback computes them once and hands
// them to both proposers.
func (in *Input) Shapes() []ShapeField {
	if in.shapes == nil {
		in.shapes = ShapesOf(in.Fields)
	}
	return in.shapes
}

// Result is a proposal and what made it.
type Result struct {
	Proposal Proposal
	// By names the proposer and its version: "heuristic/1".
	By string
	// Scores holds a model's confidences; nil for the heuristic.
	Scores *Scores
}

// Scores are a ranker's confidences in its proposal.
type Scores struct {
	Roles   map[Role]RoleScore `json:"roles,omitempty"`
	Kind    map[Kind]float64   `json:"kind,omitempty"`
	Weights string             `json:"weights,omitempty"`
}

// RoleScore is the ranker's view of one role: the path it settled on, its
// probability, the top three choices, and whether the ranker or the heuristic
// decided the role.
type RoleScore struct {
	Path string      `json:"path"`
	P    float64     `json:"p"`
	Top3 []Candidate `json:"top3,omitempty"`
	By   string      `json:"by"`
}

// Proposer proposes a mapping for a payload shape it has not seen.
type Proposer interface {
	Propose(ctx context.Context, in Input) (Result, error)
}

// Heuristic proposes with Propose. It never fails.
type Heuristic struct{}

func (Heuristic) Propose(_ context.Context, in Input) (Result, error) {
	return Result{Proposal: ProposeShapes(in.Shapes()), By: "heuristic/1"}, nil
}

// maxPrimaries caps the Primary calls in flight per Fallback, abandoned ones
// included, so a Primary that hangs cannot pile up goroutines.
const maxPrimaries = 4

// DefaultTimeout is how long Fallback waits for Primary when Timeout is not
// set.
const DefaultTimeout = 250 * time.Millisecond

// Fallback runs Primary and falls back to Secondary when it fails, panics,
// runs past Timeout (DefaultTimeout when zero), is canceled, proposes a
// mapping that does not validate against the payload, or already has
// maxPrimaries calls in flight. OnFallback, when set, is told why: "error",
// "panic", "timeout", "canceled", "invalid" or "busy", and
// "secondary_panic" when Secondary panics too and the Heuristic answers
// instead. A nil Primary, typed or not, goes straight to Secondary; a nil
// Secondary is the Heuristic. A Fallback is used by pointer and not copied
// after first use.
type Fallback struct {
	Primary, Secondary Proposer
	Timeout            time.Duration
	OnFallback         func(reason string)

	once  sync.Once
	slots chan struct{}
}

func (f *Fallback) Propose(ctx context.Context, in Input) (Result, error) {
	in.Shapes()
	if !isNil(f.Primary) {
		res, reason := f.primary(ctx, in)
		if reason == "" {
			return res, nil
		}
		f.fellBack(reason)
	}
	if isNil(f.Secondary) {
		return Heuristic{}.Propose(ctx, in)
	}
	o := call(ctx, f.Secondary, in)
	if o.panicked {
		f.fellBack("secondary_panic")
		return Heuristic{}.Propose(ctx, in)
	}
	return o.res, o.err
}

func (f *Fallback) fellBack(reason string) {
	if f.OnFallback != nil {
		f.OnFallback(reason)
	}
}

type outcome struct {
	res      Result
	err      error
	panicked bool
}

// primary returns Primary's result, or the reason it cannot be used. A
// Primary that ignores its context is abandoned at the deadline; its
// goroutine keeps its slot until it returns, and its result is dropped.
func (f *Fallback) primary(ctx context.Context, in Input) (Result, string) {
	if reason := ctxReason(ctx.Err()); reason != "" {
		return Result{}, reason
	}
	f.once.Do(func() { f.slots = make(chan struct{}, maxPrimaries) })
	select {
	case f.slots <- struct{}{}:
	default:
		return Result{}, "busy"
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan outcome, 1)
	go func() {
		defer func() { <-f.slots }()
		done <- call(pctx, f.Primary, in)
	}()
	var o outcome
	select {
	case o = <-done:
	case <-pctx.Done():
		if reason := ctxReason(ctx.Err()); reason != "" {
			return Result{}, reason
		}
		return Result{}, "timeout"
	}
	switch {
	case o.panicked:
		return Result{}, "panic"
	case ctxReason(o.err) != "":
		return Result{}, ctxReason(o.err)
	case o.err != nil:
		return Result{}, "error"
	}
	m := NewMapping(o.res.Proposal, in.shapes)
	if err := m.Validate(NewShape(in.shapes, in.Truncated, mappedPaths(m)...)); err != nil {
		return Result{}, "invalid"
	}
	return o.res, ""
}

// ctxReason names a context error: "canceled" or "timeout", or "" for any
// other error.
func ctxReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return ""
}

// call runs one proposer, turning a panic into an outcome.
func call(ctx context.Context, p Proposer, in Input) (o outcome) {
	defer func() {
		if recover() != nil {
			o = outcome{panicked: true}
		}
	}()
	o.res, o.err = p.Propose(ctx, in)
	return o
}

// isNil also catches a typed nil, such as a nil *Ranker in the interface.
func isNil(p Proposer) bool {
	if p == nil {
		return true
	}
	switch v := reflect.ValueOf(p); v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	}
	return false
}

func mappedPaths(m Mapping) []string {
	out := make([]string, 0, len(m.Paths))
	for _, p := range m.Paths {
		out = append(out, p)
	}
	return out
}
