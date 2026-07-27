package poller

import (
	"fmt"
	"strings"

	"github.com/mac-lucky/pushward-integrations/backrest/internal/backrest"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const (
	iconRunning = "arrow.triangle.2.circlepath"
	iconOK      = "checkmark.circle.fill"
	iconFail    = "xmark.circle.fill"
	iconWarn    = "exclamationmark.triangle.fill"
)

// State lines. The running ones end in an ellipsis so the Dynamic Island reads
// as in-flight even when the numbers are still empty.
//
// The wording matches relay/internal/backrest's, deliberately: someone can run
// the relay's hook provider and this bridge against the same account, and a
// check reported as "Check Passed" by one and "Check passed" by the other reads
// as two different events.
const (
	stateScanning        = "Scanning..."
	stateBackingUp       = "Backing up..."
	stateComplete        = "Complete"
	stateCompleteWarning = "Complete (warnings)"
	stateFailed          = "Failed"
	stateLostTrack       = "Interrupted"
	stateCancelled       = "Cancelled"
	statePruning         = "Pruning..."
	statePruned          = "Pruned"
	statePruneFailed     = "Prune Failed"
	stateChecking        = "Checking..."
	stateCheckPassed     = "Check Passed"
	stateCheckFailed     = "Check Failed"
)

// maxLogLines is the log template's ceiling. The server rejects a longer list,
// so the tail is trimmed to it rather than discovering the limit at runtime.
const maxLogLines = 20

// maxLogLineLen is the per-line ceiling. Restic's retry lines carry full URLs
// and run well past it.
const maxLogLineLen = 512

// activityName is the Live Activity's title: the plan for a backup, the repo
// for the repo-scoped tasks that have no plan of their own.
func activityName(op *backrest.Operation) string {
	if plan := op.PlanName(); plan != "" {
		return text.TruncateHard(plan, 100)
	}
	if op.RepoID != "" {
		return text.TruncateHard(op.RepoID, 100)
	}
	return "Backrest"
}

func subtitle(op *backrest.Operation) string {
	parts := make([]string, 0, 3)
	parts = append(parts, "Backrest")
	if plan := op.PlanName(); plan != "" {
		parts = append(parts, text.TruncateHard(plan, 50))
	}
	if op.RepoID != "" {
		parts = append(parts, text.TruncateHard(op.RepoID, 50))
	}
	return strings.Join(parts, text.SepDot)
}

// formatRate renders a transfer rate. FormatBytes already picks the unit, so
// this only has to add the denominator.
func formatRate(bytesPerSec float64) string {
	if bytesPerSec <= 0 {
		return ""
	}
	return text.FormatBytes(int64(bytesPerSec)) + "/s"
}

// backupRunningState is the line under a moving bar: how much of how much, and
// how fast. Before restic finishes scanning there is no total to divide by, so
// it says so instead of showing "0 B of 0 B".
func backupRunningState(op *backrest.Operation, speed float64) string {
	st := op.BackupStatus()
	if st == nil || st.TotalBytes <= 0 {
		return stateScanning
	}
	parts := []string{
		fmt.Sprintf("%s of %s", text.FormatBytes(st.BytesDone.Int64()), text.FormatBytes(st.TotalBytes.Int64())),
	}
	if rate := formatRate(speed); rate != "" {
		parts = append(parts, rate)
	}
	return strings.Join(parts, text.SepDot)
}

// summaryDetail drops any of the three parts restic did not report, so an
// unchanged tree reads as a duration alone rather than "0 B · 0 files · 3s".
func summaryDetail(op *backrest.Operation) string {
	s := op.BackupSummary()
	if s == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if s.DataAdded > 0 {
		parts = append(parts, text.FormatBytes(s.DataAdded.Int64()))
	}
	if n := s.FilesTouched(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d files", n))
	}
	if d, ok := op.Elapsed(); ok {
		parts = append(parts, text.FormatDuration(d))
	}
	return strings.Join(parts, text.SepDot)
}

func endStateText(op *backrest.Operation) string {
	switch op.Kind() {
	case backrest.KindPrune:
		if op.Failed() {
			return statePruneFailed
		}
		return statePruned
	case backrest.KindCheck:
		if op.Failed() {
			return stateCheckFailed
		}
		return stateCheckPassed
	}

	switch op.Status {
	case backrest.StatusSuccess:
		if d := summaryDetail(op); d != "" {
			return stateComplete + text.SepDot + d
		}
		return stateComplete
	case backrest.StatusWarning:
		if d := summaryDetail(op); d != "" {
			return stateCompleteWarning + text.SepDot + d
		}
		return stateCompleteWarning
	case backrest.StatusSystemCancelled, backrest.StatusUserCancelled:
		return stateCancelled
	}
	return stateFailed
}

func endIcon(op *backrest.Operation) (icon, color string) {
	switch {
	case op.Failed():
		return iconFail, pushward.ColorRed
	case op.Status == backrest.StatusWarning:
		return iconWarn, pushward.ColorOrange
	}
	return iconOK, pushward.ColorGreen
}

// Phases are the coarse steps a frame moves through, as opposed to the numbers
// inside its state line. Only a phase change is worth a push on its own.
const (
	phaseScanning = "scanning"
	phaseRunning  = "running"
)

// runningContent renders a backup in flight. liveStart/liveEnd are the unix
// window the bar is anchored to fill across; zero leaves the bar static, which
// is what happens before there is enough history to estimate a rate.
//
// The phase is returned rather than derived from the state line because that
// line embeds a byte count and a transfer rate, both of which move on nearly
// every tick.
func runningContent(op *backrest.Operation, speed float64, liveStart, liveEnd int64) (pushward.Content, string) {
	progress, _ := op.Progress()
	state := backupRunningState(op, speed)

	c := pushward.Content{
		Template:    pushward.TemplateGeneric,
		Progress:    progress,
		State:       state,
		Icon:        iconRunning,
		Subtitle:    subtitle(op),
		AccentColor: pushward.ColorBlue,
	}
	if liveEnd > 0 {
		c.LiveProgress = pushward.BoolPtr(true)
		c.StartDate = pushward.Int64Ptr(liveStart)
		c.EndDate = pushward.Int64Ptr(liveEnd)
	}

	phase := phaseRunning
	if state == stateScanning {
		phase = phaseScanning
	}
	return c, phase
}

// repoTaskContent renders a running prune or check. Neither reports progress -
// the proto has no percent field for them at all - so the bar stays at zero and
// the log carries the detail.
func repoTaskContent(op *backrest.Operation, lines []pushward.LogLine) pushward.Content {
	state := statePruning
	if op.Kind() == backrest.KindCheck {
		state = stateChecking
	}
	return withLogLines(pushward.Content{
		Template:     pushward.TemplateGeneric,
		Progress:     0,
		State:        state,
		Icon:         iconRunning,
		Subtitle:     subtitle(op),
		AccentColor:  pushward.ColorBlue,
		LiveProgress: pushward.BoolPtr(false),
	}, lines)
}

// endContent renders a finished operation. A failure with something to show -
// per-file errors, or restic's own output - becomes a log view, because the
// list of what broke is the actionable half and a single state line cannot hold
// it.
func endContent(op *backrest.Operation, lines []pushward.LogLine) pushward.Content {
	icon, color := endIcon(op)
	return withLogLines(pushward.Content{
		Template:     pushward.TemplateGeneric,
		Progress:     endProgress(op),
		State:        endStateText(op),
		Icon:         icon,
		Subtitle:     subtitle(op),
		AccentColor:  color,
		LiveProgress: pushward.BoolPtr(false),
	}, lines)
}

// orphanContent renders an operation that left the query window without ever
// reporting an outcome.
//
// Orange rather than red, and "Interrupted" rather than "Failed": nothing is
// known to have gone wrong, the bridge simply lost sight of it. The bar is left
// where it stopped for the same reason endProgress leaves a failure there.
func orphanContent(subtitle string, progress float64) pushward.Content {
	return pushward.Content{
		Template:     pushward.TemplateGeneric,
		Progress:     progress,
		State:        stateLostTrack,
		Icon:         iconWarn,
		Subtitle:     subtitle,
		AccentColor:  pushward.ColorOrange,
		LiveProgress: pushward.BoolPtr(false),
	}
}

// endProgress is where the bar comes to rest. A completed operation fills it; a
// failed one stops where restic stopped, because a backup that died at 94% and
// a backup that never started are different events and a bar snapped back to
// zero would render them identically.
func endProgress(op *backrest.Operation) float64 {
	if !op.Failed() {
		return 1.0
	}
	if p, ok := op.Progress(); ok {
		return p
	}
	return 0
}

// withLogLines upgrades a frame to the log template when there is output worth
// showing, and leaves it alone when there is not.
//
// Callers must set LiveProgress explicitly, including when it is false: these
// bodies are merge-patched onto whatever the last tick sent, and an omitted
// field is preserved, so a finished backup would inherit the running frame's
// live_progress and keep animating a bar that has stopped moving.
func withLogLines(c pushward.Content, lines []pushward.LogLine) pushward.Content {
	if len(lines) > 0 {
		c.Template = pushward.TemplateLog
		c.Lines = lines
	}
	return c
}

// errorLines turns restic's per-file failures into log lines. These survive a
// successful backup: restic reports a snapshot and still lists what it could
// not read.
func errorLines(op *backrest.Operation) []pushward.LogLine {
	if op.Backup == nil || len(op.Backup.Errors) == 0 {
		return nil
	}
	lines := make([]pushward.LogLine, 0, maxLogLines)
	for _, e := range op.Backup.Errors {
		if len(lines) == maxLogLines {
			break
		}
		msg := e.Item
		if e.Message != "" {
			msg = e.Item + ": " + e.Message
		}
		if msg == "" {
			continue
		}
		lines = append(lines, pushward.LogLine{
			Text:  truncateLine(msg),
			Level: pushward.LogError,
		})
	}
	return lines
}

// outputLines turns command output into log lines: the last maxLogLines of it,
// newest first, which is the order the log template renders.
func outputLines(output string) []pushward.LogLine {
	raw := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")

	// Walk backwards so the tail is what survives, skipping the blank lines
	// restic uses to separate its summary blocks - they would spend entries of
	// a 20-line budget saying nothing.
	lines := make([]pushward.LogLine, 0, maxLogLines)
	for i := len(raw) - 1; i >= 0 && len(lines) < maxLogLines; i-- {
		s := strings.TrimRight(raw[i], " \t\r")
		if strings.TrimSpace(s) == "" {
			continue
		}
		lines = append(lines, pushward.LogLine{
			Text:  truncateLine(s),
			Level: lineLevel(s),
		})
	}
	return lines
}

// lineLevel classifies a log line by what restic writes when things go wrong.
// "error" also matches "no errors were found", so the negations are checked
// first - a clean check reporting itself as an error would be worse than no
// classification at all.
func lineLevel(s string) string {
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "no errors"), strings.Contains(l, "0 errors"):
		return pushward.LogInfo
	case strings.Contains(l, "error"), strings.Contains(l, "failed"), strings.Contains(l, "fatal"):
		return pushward.LogError
	case strings.Contains(l, "warning"), strings.Contains(l, "retrying"):
		return pushward.LogWarn
	}
	return pushward.LogInfo
}

func truncateLine(s string) string {
	return text.TruncateHard(s, maxLogLineLen)
}
