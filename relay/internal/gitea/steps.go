package gitea

import "strings"

// Gitea job statuses (mirrors GitHub, with Gitea's own naming for the pre-run
// states: "queued" and "blocked/waiting" both mean not-yet-running here).
const (
	jobStatusCompleted  = "completed"
	jobStatusInProgress = "in_progress"
)

// jobFailed reports whether a completed job's conclusion indicates failure.
// "skipped" is not a failure.
func jobFailed(conclusion string) bool {
	switch conclusion {
	case "failure", "cancelled", "timed_out", "startup_failure":
		return true
	}
	return false
}

// stepInfo holds computed steps-template information from a set of jobs.
type stepInfo struct {
	TotalSteps      int
	CurrentStep     int
	CurrentStepName string
	StepRows        []int
	StepLabels      []string
	AllCompleted    bool
	AnyFailed       bool
	Progress        float64
}

// computeSteps groups jobs by base name (folding matrix strategies into one
// group) and computes step progress. Ported from the GitHub Actions poller so
// Gitea renders the same steps ladder as the GitHub bridge.
func computeSteps(jobs []jobRecord) stepInfo {
	type step struct {
		name      string
		count     int
		completed int
		active    bool
		failed    bool
	}
	var steps []step
	stepIdx := make(map[string]int)
	completedJobs := 0
	allCompleted := true
	anyFailed := false

	for _, job := range jobs {
		base := baseJobName(job.Name)
		si, ok := stepIdx[base]
		if !ok {
			si = len(steps)
			stepIdx[base] = si
			steps = append(steps, step{name: base})
		}
		steps[si].count++

		switch job.Status {
		case jobStatusCompleted:
			completedJobs++
			steps[si].completed++
			if jobFailed(job.Conclusion) {
				steps[si].failed = true
				anyFailed = true
			}
		case jobStatusInProgress:
			steps[si].active = true
			allCompleted = false
		default: // queued / waiting / blocked
			allCompleted = false
		}
	}

	totalSteps := len(steps)
	stepRows := make([]int, totalSteps)
	stepLabels := make([]string, totalSteps)
	currentStep := 0
	var currentStepName string

	for i, s := range steps {
		stepRows[i] = s.count
		stepLabels[i] = s.name
		if s.active && currentStepName == "" {
			currentStepName = s.name
			currentStep = i + 1
		}
	}

	if currentStepName == "" && !allCompleted {
		currentStepName = "Queued"
		for i, s := range steps {
			if s.completed < s.count {
				currentStep = i + 1
				break
			}
		}
	}

	progress := 0.0
	if len(jobs) > 0 {
		progress = float64(completedJobs) / float64(len(jobs))
	}

	return stepInfo{
		TotalSteps:      totalSteps,
		CurrentStep:     currentStep,
		CurrentStepName: currentStepName,
		StepRows:        stepRows,
		StepLabels:      stepLabels,
		AllCompleted:    allCompleted,
		AnyFailed:       anyFailed,
		Progress:        progress,
	}
}

// baseJobName strips the reusable-workflow caller prefix and matrix parameters
// from a job name.
//
//	"ci-cd / Build (ubuntu, node-16)" -> "Build"
//	"ci-cd / Setup Build Environment"  -> "Setup Build Environment"
//	"Test" -> "Test"
func baseJobName(name string) string {
	if i := strings.Index(name, " / "); i != -1 {
		name = name[i+3:]
	}
	if i := strings.LastIndex(name, " ("); i != -1 && strings.HasSuffix(name, ")") {
		return name[:i]
	}
	return name
}
