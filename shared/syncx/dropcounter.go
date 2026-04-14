// Package syncx provides small, focused concurrency primitives shared
// across pushward-integrations.
package syncx

import "sync/atomic"

// DropCounter counts dropped items and reports whether the caller should
// emit a log on the first drop and every Nth drop thereafter. Zero value
// is not valid — use NewDropCounter.
type DropCounter struct {
	n        atomic.Uint64
	logEvery uint64
}

// NewDropCounter returns a DropCounter that reports shouldLog on the 1st
// drop and every logEvery drops after that. Panics if logEvery is zero.
func NewDropCounter(logEvery uint64) *DropCounter {
	if logEvery == 0 {
		panic("syncx: NewDropCounter requires logEvery > 0")
	}
	return &DropCounter{logEvery: logEvery}
}

// Drop increments the counter and returns the new total along with
// whether the caller should log this drop.
func (c *DropCounter) Drop() (total uint64, shouldLog bool) {
	total = c.n.Add(1)
	return total, total == 1 || total%c.logEvery == 0
}
