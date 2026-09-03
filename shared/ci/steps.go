// Package ci renders a CI forge's job list into the PushWard steps ladder.
//
// Three callers feed it: the github and forgejo pollers, and the relay's
// gitea/forgejo webhook handler. Each converts its own wire shape into []Job and
// reads back a StepInfo; nothing forge-specific lives here.
//
// The vocabulary below is GitHub Actions'. Two of the three callers already
// speak it on the wire, so adopting it verbatim means only the forgejo client
// translates, and it does so once at its own boundary.
package ci

import (
	"slices"
	"strings"
	"time"
)

// Job statuses. Any value other than these two is treated as not-yet-running,
// which covers GitHub's "queued" as well as Forgejo's "waiting" and "blocked".
const (
	StatusQueued     = "queued"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
)

// Conclusions for a completed job. Skipped is deliberately not a failure.
const (
	ConclusionSuccess        = "success"
	ConclusionFailure        = "failure"
	ConclusionCancelled      = "cancelled"
	ConclusionSkipped        = "skipped"
	ConclusionTimedOut       = "timed_out"
	ConclusionStartupFailure = "startup_failure"
)

// Job is one CI job, forge-neutral.
//
// StartedAt and CompletedAt are zero when the forge has not stamped them, or
// does not expose them at all: the relay's webhook payloads never carry
// timestamps, and Forgejo's jobs endpoint omits them until a separate task
// lookup fills them in. Every consumer here reads zero as "unknown" and never as
// the epoch, so an unstamped job costs a measurement rather than producing a
// wrong one.
type Job struct {
	Name        string
	Status      string
	Conclusion  string
	StartedAt   time.Time
	CompletedAt time.Time
}

// StepInfo is the computed steps-template shape for a set of jobs.
type StepInfo struct {
	TotalSteps      int
	CurrentStep     int
	CurrentStepName string
	StepRows        []int
	StepLabels      []string
	StepColors      []string

	// CurrentStepStartedAt is when the current group began - its earliest
	// stamped start across every shard, finished ones included - and is the
	// point LiveAnchor measures its window from; GroupWeights spans from the
	// same start. Zero when the forge has stamped none of the group's jobs.
	CurrentStepStartedAt time.Time

	AllCompleted bool
	AnyFailed    bool
	Progress     float64
}

// JobFailed reports whether a completed job's conclusion indicates failure.
func JobFailed(conclusion string) bool {
	switch conclusion {
	case ConclusionFailure, ConclusionCancelled, ConclusionTimedOut, ConclusionStartupFailure:
		return true
	}
	return false
}

// QueuedStepName is the placeholder ComputeSteps reports when a run has revealed
// jobs but none of them is running yet. It is not a job group: no forge measured
// it and nothing is executing under it, so LiveAnchor refuses to animate it even
// when the rest of the run is measured. The step labels themselves are clamped to
// the server's bounds at the wire, by pushward.ClampStepShape.
const QueuedStepName = "Queued"

// ComputeSteps groups jobs by base name (folding matrix strategies into one
// group) and computes step progress.
func ComputeSteps(jobs []Job) StepInfo {
	type step struct {
		name      string
		count     int
		completed int
		active    bool
		failed    bool
		// startedAt is the earliest start among the group's jobs, running or
		// already finished: a fan-out group's shards run against one step
		// deadline, so the step started when its first shard did, even one that
		// has come and gone by the time a poll lands.
		startedAt time.Time
	}
	var steps []step
	stepIdx := make(map[string]int)
	completedJobs := 0
	allCompleted := true
	anyFailed := false

	for _, job := range jobs {
		base := BaseJobName(job.Name)
		si, ok := stepIdx[base]
		if !ok {
			si = len(steps)
			stepIdx[base] = si
			steps = append(steps, step{name: base})
		}
		steps[si].count++
		steps[si].startedAt = earliest(steps[si].startedAt, job.StartedAt)

		switch job.Status {
		case StatusCompleted:
			completedJobs++
			steps[si].completed++
			if JobFailed(job.Conclusion) {
				steps[si].failed = true
				anyFailed = true
			}
		case StatusInProgress:
			steps[si].active = true
			allCompleted = false
		default: // queued
			allCompleted = false
		}
	}

	totalSteps := len(steps)
	stepRows := make([]int, totalSteps)
	stepLabels := make([]string, totalSteps)
	stepColors := make([]string, totalSteps)
	currentStep := 0
	var currentStepName string

	for i, s := range steps {
		stepRows[i] = s.count
		stepLabels[i] = s.name
		stepColors[i] = StepColor(s.name)
		if s.active && currentStepName == "" {
			currentStepName = s.name
			currentStep = i + 1
		}
	}

	if currentStepName == "" && !allCompleted {
		currentStepName = QueuedStepName
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

	var currentStartedAt time.Time
	if currentStep >= 1 {
		currentStartedAt = steps[currentStep-1].startedAt
	}

	return StepInfo{
		TotalSteps:           totalSteps,
		CurrentStep:          currentStep,
		CurrentStepName:      currentStepName,
		StepRows:             stepRows,
		StepLabels:           stepLabels,
		StepColors:           stepColors,
		CurrentStepStartedAt: currentStartedAt,
		AllCompleted:         allCompleted,
		AnyFailed:            anyFailed,
		Progress:             progress,
	}
}

// StepColor maps a job-group name to a Live Activity step color so the steps bar
// reads at a glance: tests one hue, build another, deploy another. The match is
// substring-based on the lowercased base job name; an unmatched group returns ""
// and falls back to the activity accent color. Colors are named values the iOS
// client and server both accept.
//
// Case order is load-bearing: "Docker Build" matches build before docker.
func StepColor(name string) string {
	n := strings.ToLower(name)
	switch {
	case containsAny(n, "test", "e2e", "pytest", "jest", "vitest"):
		return "yellow"
	case containsAny(n, "lint", "format", "typecheck", "golangci", "gofmt", "ruff"):
		return "purple"
	case containsAny(n, "build", "compile", "assemble"):
		return "blue"
	case containsAny(n, "docker", "image", "container", "buildx"):
		return "cyan"
	case containsAny(n, "deploy", "release", "publish"):
		return "green"
	case containsAny(n, "security", "scan", "codeql", "trivy", "grype", "sast"):
		return "orange"
	default:
		return ""
	}
}

// containsAny reports whether s contains any of the given substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// BaseJobName strips the reusable-workflow caller prefix and matrix parameters
// from a job name.
//
//	"ci-cd / Build (ubuntu, node-16)" -> "Build"
//	"ci-cd / Setup Build Environment" -> "Setup Build Environment"
//	"Test"                            -> "Test"
func BaseJobName(name string) string {
	// Strip reusable-workflow caller prefix ("ci-cd / X" -> "X").
	if i := strings.Index(name, " / "); i != -1 {
		name = name[i+3:]
	}
	// Strip matrix parameters ("Build (ubuntu, node-16)" -> "Build").
	if i := strings.LastIndex(name, " ("); i != -1 && strings.HasSuffix(name, ")") {
		return name[:i]
	}
	return name
}

// RealignStep moves a 1-based step index from one label list onto another by
// group name. The live scan numbers groups in the order the forge revealed them,
// so a run that skips an if-gated job leaves the seeded list with an extra entry
// ahead of the running one and the raw index lands on the wrong label. iOS draws
// both the caption and the highlighted pill from step_labels[i-1], so a stale
// index makes the card name one group while its state text names another.
// Falls back to the original index when the group is not in the target list,
// which is no worse than not remapping at all.
func RealignStep(current int, from, to []string) int {
	if current < 1 || current > len(from) {
		return current
	}
	if i := slices.Index(to, from[current-1]); i >= 0 {
		return i + 1
	}
	return current
}
