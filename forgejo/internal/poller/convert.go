package poller

import (
	fjclient "github.com/mac-lucky/pushward-integrations/forgejo/internal/forgejo"
	"github.com/mac-lucky/pushward-integrations/shared/ci"
)

// toCIJobs converts the client's normalized jobs into the shared ladder's input
// shape. This is a plain field copy: the client already translated Forgejo's
// single status field into the status/conclusion pair and joined the timestamps
// in from the tasks endpoint, so there is no forge-specific logic left here.
//
// Timestamps that the tasks join could not supply stay zero, which the ladder
// reads as "unknown" - it floors the group's weight and declines to anchor,
// rather than inventing either.
func toCIJobs(jobs []fjclient.Job) []ci.Job {
	out := make([]ci.Job, len(jobs))
	for i, j := range jobs {
		out[i] = ci.Job{
			Name:        j.Name,
			Status:      j.Status,
			Conclusion:  j.Conclusion,
			StartedAt:   j.StartedAt,
			CompletedAt: j.CompletedAt,
		}
	}
	return out
}
