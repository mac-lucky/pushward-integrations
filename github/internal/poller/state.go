package poller

import "time"

type trackedRun struct {
	Repo       string
	RunID      int64
	Name       string
	Branch     string
	StartedAt  time.Time
	LastUpdate time.Time
}
