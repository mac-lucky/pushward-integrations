package cipoll

import (
	"slices"
	"sync"

	"github.com/mac-lucky/pushward-integrations/shared/ci"
)

// maxSeeds bounds the seed cache: one entry per repo and workflow this process
// has watched to completion. Past the cap the oldest entry goes, and the next run
// of that workflow seeds from the forge instead - which is where every entry
// started, so nothing is lost but a lookup.
const maxSeeds = 256

type seedKey struct{ repo, workflow string }

// seedEntry is what a finished run leaves behind for the next run of its
// workflow: the shape it actually revealed, and how long each group held it up.
type seedEntry struct {
	// shape is the observed final scan of the run - total, rows, labels, colors -
	// not the clamped maximum the card displayed, which may have carried phantom
	// steps over from its own seed.
	shape ci.StepInfo
	// weights is ci.GroupWeights over the run's jobs as the bridge last saw them,
	// nil when nothing was measurable. Never mutated once stored: readers hand
	// the same map straight to trackedRun.stepWeightByName.
	weights map[string]float64
	runID   int64
	success bool
}

// shapeCache remembers the last run this process saw finish, per repo and
// workflow, so the next run of that workflow seeds from it rather than from a
// forge lookup. Beyond the requests it saves: the run was measured live, from
// timestamps read minutes after the jobs stopped, which no later rewrite of the
// forge's task rows can reach; and it is keyed by workflow alone, so a tag
// build or a pull request seeds from the run that just went by on another ref.
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
func (c *shapeCache) put(repo, workflow string, shape ci.StepInfo, weights map[string]float64, runID int64, success bool) {
	if workflow == "" || shape.TotalSteps <= 0 {
		return
	}
	key := seedKey{repo: repo, workflow: workflow}

	c.mu.Lock()
	defer c.mu.Unlock()

	if prev, ok := c.entries[key]; ok {
		if prev.success && !success {
			return
		}
		if slices.Equal(prev.shape.StepLabels, shape.StepLabels) {
			weights = mergeWeights(weights, prev.weights)
		}
		c.order = slices.DeleteFunc(c.order, func(k seedKey) bool { return k == key })
	}
	c.entries[key] = seedEntry{shape: copyShape(shape), weights: weights, runID: runID, success: success}
	c.order = append(c.order, key)
	for len(c.order) > c.capacity {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
}

// get returns the stored run. Readers copy what they keep and never write
// through the slices, so the entry is handed out as stored.
func (c *shapeCache) get(repo, workflow string) (seedEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[seedKey{repo: repo, workflow: workflow}]
	return entry, ok
}

// mergeWeights fills the groups fresh could not measure from prev, the stored
// measurement of the same labels. Neither input is mutated; the result is nil
// only when both are.
func mergeWeights(fresh, prev map[string]float64) map[string]float64 {
	if prev == nil {
		return fresh
	}
	if fresh == nil {
		return prev
	}
	out := make(map[string]float64, len(fresh))
	for name, w := range fresh {
		if w <= ci.StepWeightFloor && prev[name] > ci.StepWeightFloor {
			w = prev[name]
		}
		out[name] = w
	}
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
