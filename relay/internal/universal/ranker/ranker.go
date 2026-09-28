// Package ranker is a logistic-regression ranker over the universal
// heuristic's candidates. For every role it scores the heuristic's top TopK
// fields plus none and takes a softmax over them; a three-way model does the
// same for the kind. Weights are trained offline (pushward-classifier, `just
// retrain`) and embedded; they ship with a gate verdict and a per-role serve
// flag, and roles the ranker did not win on keep the heuristic's pick.
package ranker

import (
	"cmp"
	"context"
	"math"
	"slices"

	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
)

// Ranker proposes mappings with the embedded weights. Build it with New.
type Ranker struct {
	id   string
	by   string
	pass bool
	// roles and serve are indexed like universal.Roles.
	roles     []roleModel
	serve     []bool
	kind      kindModel
	serveKind bool
}

type roleModel struct {
	intercept, temp float64
	w               map[string]float64
}

type kindModel struct {
	classes   []universal.Kind
	intercept []float64
	temp      float64
	w         map[string][]float64
}

// New returns the ranker, or false when the embedded weights did not pass
// their gate or do not fit this build (schema, feature version, option
// count). The caller then keeps the heuristic.
func New() (universal.Proposer, bool) {
	return proposer(load())
}

func proposer(r *Ranker, err error) (universal.Proposer, bool) {
	if err != nil || r == nil || !r.pass {
		return nil, false
	}
	return r, true
}

// Info describes the embedded weights for a startup log line: their id,
// whether they passed their gate, and why they do not load, if they do not.
func Info() (id string, pass bool, err error) {
	r, err := load()
	if err != nil {
		return "", false, err
	}
	return r.id, r.pass, nil
}

// Propose scores every role's options, then builds the mapping in Roles order
// under the heuristic's rule that a field plays one role, correlation and url
// excepted. A role the weights do not serve takes the heuristic's pick among
// the fields still free, so with nothing served the result is the
// heuristic's own proposal.
func (r *Ranker) Propose(ctx context.Context, in universal.Input) (universal.Result, error) {
	if err := ctx.Err(); err != nil {
		return universal.Result{}, err
	}
	shapes := in.Shapes()
	s := r.score(Extract(shapes))
	p, by := r.compose(s)
	p.SetTables(shapes)
	return universal.Result{Proposal: p, By: r.by, Scores: r.scores(s, &p, by)}, nil
}

// scored holds the models' outputs for one payload, per role in Roles order
// and per option.
type scored struct {
	ex            *Extraction
	logits, probs [][]float64
	kindLogits    []float64
	kindProbs     []float64
}

func (r *Ranker) score(ex *Extraction) *scored {
	s := &scored{ex: ex, logits: make([][]float64, len(ex.Options)), probs: make([][]float64, len(ex.Options))}
	for i, opts := range ex.Options {
		m := &r.roles[i]
		z := make([]float64, len(opts))
		for j := range opts {
			z[j] = m.logit(opts[j].Features)
		}
		s.logits[i], s.probs[i] = z, softmax(z, m.temp)
	}
	s.kindLogits = r.kind.logits(ex.Kind)
	s.kindProbs = softmax(s.kindLogits, r.kind.temp)
	return s
}

func (m *roleModel) logit(fs Features) float64 {
	z := m.intercept
	for _, f := range fs {
		if w, ok := m.w[f.Name]; ok {
			// The conversion keeps the compiler from fusing a multiply-add,
			// so the sum rounds as the Python scorer's does.
			z += float64(w * f.Value)
		}
	}
	return z
}

func (m *kindModel) logits(fs Features) []float64 {
	z := slices.Clone(m.intercept)
	for _, f := range fs {
		if w, ok := m.w[f.Name]; ok {
			for c := range z {
				z[c] += float64(w[c] * f.Value)
			}
		}
	}
	return z
}

// softmax returns softmax(z / t).
func softmax(z []float64, t float64) []float64 {
	p := make([]float64, len(z))
	top := math.Inf(-1)
	for i, v := range z {
		p[i] = v / t
		top = max(top, p[i])
	}
	sum := 0.0
	for i := range p {
		p[i] = math.Exp(p[i] - top)
		sum += p[i]
	}
	for i := range p {
		p[i] /= sum
	}
	return p
}

// byProb returns option indexes, most probable first; ties keep option
// order.
func byProb(p []float64) []int {
	idx := make([]int, len(p))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return cmp.Compare(p[b], p[a]) })
	return idx
}

// argmax returns the first index of the largest value.
func argmax(p []float64) int {
	best := 0
	for i, v := range p {
		if v > p[best] {
			best = i
		}
	}
	return best
}

// Who decided a role, in RoleScore.By.
const (
	byRanker    = "ranker"
	byHeuristic = "heuristic"
)

// compose builds the served proposal, without value tables. It returns who
// decided each role, in Roles order.
func (r *Ranker) compose(s *scored) (universal.Proposal, []string) {
	var p universal.Proposal
	by := make([]string, len(universal.Roles))
	used := map[string]bool{}
	for i, role := range universal.Roles {
		pick := ""
		if r.serve[i] {
			by[i] = byRanker
			opts := s.ex.Options[i]
			for _, j := range byProb(s.probs[i]) {
				path := opts[j].Path
				if path == "" || !used[path] || universal.Reusable(role) {
					pick = path
					break
				}
			}
		} else {
			by[i] = byHeuristic
			for _, c := range s.ex.Ranked[i] {
				if c.Score < universal.MinScore(role) {
					break
				}
				if !used[c.Path] || universal.Reusable(role) {
					pick = c.Path
					break
				}
			}
		}
		p.SetPath(role, pick)
		if pick != "" {
			used[pick] = true
		}
	}
	p.Kind = s.ex.Proposal.Kind
	if r.serveKind {
		p.Kind = r.kind.classes[argmax(s.kindProbs)]
	}
	return p, by
}

func (r *Ranker) scores(s *scored, p *universal.Proposal, by []string) *universal.Scores {
	out := &universal.Scores{
		Roles:   make(map[universal.Role]universal.RoleScore, len(universal.Roles)),
		Kind:    make(map[universal.Kind]float64, len(r.kind.classes)),
		Weights: r.id,
	}
	for i, role := range universal.Roles {
		opts, probs := s.ex.Options[i], s.probs[i]
		rs := universal.RoleScore{Path: p.Path(role), By: by[i]}
		for j := range opts {
			if opts[j].Path == rs.Path {
				rs.P = probs[j]
				break
			}
		}
		order := byProb(probs)
		for _, j := range order[:min(len(order), 3)] {
			rs.Top3 = append(rs.Top3, universal.Candidate{Path: opts[j].Path, Score: probs[j]})
		}
		out.Roles[role] = rs
	}
	for c, k := range r.kind.classes {
		out.Kind[k] = s.kindProbs[c]
	}
	return out
}
