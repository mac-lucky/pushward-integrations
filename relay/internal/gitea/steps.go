package gitea

import "github.com/mac-lucky/pushward-integrations/shared/ci"

// toCIJobs converts stored Gitea job records into the shared ladder's input
// shape. Gitea's workflow_job webhook already speaks the GitHub vocabulary
// ("completed"/"in_progress", and "failure"/"cancelled" for a failing
// conclusion), so status and conclusion pass through untranslated.
//
// StartedAt and CompletedAt stay zero: no webhook fires on step transitions and
// the payload carries no timestamps, so there is nothing to measure. The ladder
// reads zero as "unknown" and renders the static bar, which is exactly what this
// provider has always sent - it never emits step_weights or live_progress.
func toCIJobs(jobs []jobRecord) []ci.Job {
	out := make([]ci.Job, len(jobs))
	for i, j := range jobs {
		out[i] = ci.Job{
			Name:       j.Name,
			Status:     j.Status,
			Conclusion: j.Conclusion,
		}
	}
	return out
}
