package poller

import (
	"time"

	ghclient "github.com/mac-lucky/pushward-integrations/github/internal/github"
	"github.com/mac-lucky/pushward-integrations/shared/ci"
)

// toCIJobs converts GitHub's wire jobs into the shared ladder's input shape.
// GitHub sends its job timestamps as RFC3339 strings, so parsing them is the
// only GitHub-specific piece of the ladder and it stays here rather than in
// shared/ci.
func toCIJobs(jobs []ghclient.Job) []ci.Job {
	out := make([]ci.Job, len(jobs))
	for i, j := range jobs {
		out[i] = ci.Job{
			Name:        j.Name,
			Status:      j.Status,
			Conclusion:  j.Conclusion,
			StartedAt:   parseJobTime(j.StartedAt),
			CompletedAt: parseJobTime(j.CompletedAt),
		}
	}
	return out
}

// parseJobTime parses one of GitHub's RFC3339 job timestamps, returning the
// zero time when it is absent or malformed. The ladder reads a zero time as
// "unknown", so a malformed stamp costs a measurement instead of fabricating
// one.
func parseJobTime(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}
	}
	return t
}
