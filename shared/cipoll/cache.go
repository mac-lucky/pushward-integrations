package cipoll

import (
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/ci"
)

// maxSeeds bounds the seed cache: one entry per repo and workflow this process
// has watched to completion. Past the cap the oldest entry goes, and the next run
// of that workflow seeds from the forge instead - which is where every entry
// started, so nothing is lost but a lookup.
const maxSeeds = 1024

type seedKey struct{ repo, workflow, ref string }

// seedEntry is what a finished run leaves behind for the next run of its
// workflow: the shape it actually revealed, and how long each group held it up.
type seedEntry struct {
	// shape is the observed final scan of the run - total, rows, labels, colors -
	// not the clamped maximum the card displayed, which may have carried phantom
	// steps over from its own seed.
	shape ci.StepInfo
	// weights is ci.GroupWeights over the run's jobs as the bridge last saw them:
	// a group it could not time is absent, and nil when nothing was measurable.
	// Never mutated once stored: readers hand the same map straight to
	// trackedRun.stepWeightByName.
	weights map[string]float64
	// duration is how long the run took, from its creation to the tick that saw
	// it finish, so a run nothing could be measured on still seeds an even
	// split, and a group the join missed takes its share (see ci.FillWeights).
	// Zero when the forge reported no creation time.
	duration time.Duration
	runID    int64
	success  bool
}

// seedWeights is what the entry contributes to the next run: what was measured,
// clamped to the run, with the run's length spread over the rest. Built on
// every read and never stored back, so a share can never masquerade as a
// measurement when the next run's weights are merged in.
func (e seedEntry) seedWeights() (map[string]float64, ci.WeightsSource) {
	return ci.FillWeights(e.weights, e.shape.StepLabels, e.duration)
}

// seedMatch says how a lookup was satisfied, in the vocabulary the forge rung's
// log already uses. Blank is a miss.
type seedMatch string

const (
	seedMiss    seedMatch = ""
	seedSameRef seedMatch = "same ref"
	seedAnyRef  seedMatch = "any ref"
)

// shapeCache remembers the last run this process saw finish, per repo,
// workflow and ref, so the next run of that workflow seeds from it rather than
// from a forge lookup. Beyond the requests it saves: the run was measured live,
// from timestamps read minutes after the jobs stopped, which no later rewrite
// of the forge's task rows can reach. A lookup prefers the run's own ref and
// falls back to the workflow's newest run on any ref, so a tag build or a pull
// request still seeds from the run that just went by on another branch, while
// a ref with a run of its own - a path of the workflow with other jobs - keeps
// its own shape.
//
// A leaf: its methods are never called under Poller.mu, and it never calls back
// into the loop. In-memory only; a restart falls back to the forge.
type shapeCache struct {
	mu      sync.Mutex
	entries map[seedKey]seedEntry
	// order is insertion order, oldest first, for eviction. An overwrite moves
	// its key to the end.
	order    []seedKey
	capacity int
}

func newShapeCache(capacity int) *shapeCache {
	return &shapeCache{entries: make(map[seedKey]seedEntry), capacity: capacity}
}

// put stores a finished run, with two guards against replacing a good
// measurement with a worse one. A run that did not succeed never displaces one
// that did: its failing job stopped early and everything behind it never ran,
// so its durations would count the next run down to the wrong deadline. And
// when the labels match, a group the new run could not measure keeps the stored
// measurement - the final tick's task page can come back short on a busy repo,
// and one such tick should not throw away what the same shape already measured.
func (c *shapeCache) put(repo, workflow, ref string, e seedEntry) {
	if workflow == "" || e.shape.TotalSteps <= 0 {
		return
	}
	key := seedKey{repo: repo, workflow: workflow, ref: ref}

	c.mu.Lock()
	defer c.mu.Unlock()

	if prev, ok := c.entries[key]; ok {
		if prev.success && !e.success {
			return
		}
		if slices.Equal(prev.shape.StepLabels, e.shape.StepLabels) {
			e.weights = mergeWeights(e.weights, prev.weights)
		}
		c.order = slices.DeleteFunc(c.order, func(k seedKey) bool { return k == key })
	}
	e.shape = copyShape(e.shape)
	c.entries[key] = e
	c.order = append(c.order, key)
	for len(c.order) > c.capacity {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
}

// get returns the stored run for the workflow: the one on the run's own ref,
// else the newest on any ref, preferring a successful one the way the forge's
// any-ref rung does, so a failure on one branch does not outrank a success on
// another. The any-ref scan walks order, newest first, over at most maxSeeds
// entries once per run detected, which is cheaper than a second index kept in
// step with eviction. Readers copy what they keep and never write through the
// slices, so the entry is handed out as stored.
func (c *shapeCache) get(repo, workflow, ref string) (seedEntry, seedMatch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[seedKey{repo: repo, workflow: workflow, ref: ref}]; ok {
		return entry, seedSameRef
	}
	var found seedEntry
	match := seedMiss
	for i := len(c.order) - 1; i >= 0; i-- {
		k := c.order[i]
		if k.repo != repo || k.workflow != workflow {
			continue
		}
		entry := c.entries[k]
		if entry.success {
			return entry, seedAnyRef
		}
		if match == seedMiss {
			found, match = entry, seedAnyRef
		}
	}
	return found, match
}

// mergeWeights fills the groups fresh could not measure from prev, the stored
// measurement of the same labels. A group is unmeasured by being absent, so
// this is an overlay: fresh wins wherever it measured, floor values included,
// and prev supplies the rest. Neither input is mutated; the result is nil only
// when both are.
func mergeWeights(fresh, prev map[string]float64) map[string]float64 {
	if prev == nil {
		return fresh
	}
	if fresh == nil {
		return prev
	}
	out := maps.Clone(prev)
	maps.Copy(out, fresh)
	return out
}

// copyShape keeps only the shape fields and gives them fresh backing arrays, so
// a stored entry can neither alias the caller's slices nor carry a per-tick
// scalar along with it.
func copyShape(s ci.StepInfo) ci.StepInfo {
	return ci.StepInfo{
		TotalSteps: s.TotalSteps,
		StepRows:   slices.Clone(s.StepRows),
		StepLabels: slices.Clone(s.StepLabels),
		StepColors: slices.Clone(s.StepColors),
	}
}
