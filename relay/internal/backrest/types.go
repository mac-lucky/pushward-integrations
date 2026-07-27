package backrest

import "time"

// backrestPayload is the JSON body produced by a Backrest hook template. The
// template lives in the user's own config and cannot be migrated for them, so
// every field except event is optional and an older template has to keep
// working.
//
// The stats mirror Backrest's HookVars.SnapshotStats (restic's backup summary).
// They are pointers to tell an omitted field from a real zero: otherwise a
// template that sends no stats makes every backup look like it moved nothing.
type backrestPayload struct {
	Event      string `json:"event"`
	Task       string `json:"task"`
	Plan       string `json:"plan"`
	Repo       string `json:"repo"`
	SnapshotID string `json:"snapshot_id"`
	Error      string `json:"error"`
	DurationMs int64  `json:"duration_ms"`

	DataAdded           *int64   `json:"data_added"`
	FilesNew            *int64   `json:"files_new"`
	FilesChanged        *int64   `json:"files_changed"`
	FilesUnmodified     *int64   `json:"files_unmodified"`
	TotalFilesProcessed *int64   `json:"total_files_processed"`
	TotalBytesProcessed *int64   `json:"total_bytes_processed"`
	TotalDuration       *float64 `json:"total_duration"`
}

// filesTouched returns how many files the snapshot actually backed up: new plus
// changed. total_files_processed is not that number: restic counts every file
// it walked, so on a mostly-unchanged tree it reads in the tens of thousands
// next to a few MB added. It stands in only for a template that sends nothing
// else.
func (p *backrestPayload) filesTouched() (int64, bool) {
	if p.FilesNew != nil || p.FilesChanged != nil {
		var n int64
		if p.FilesNew != nil {
			n += *p.FilesNew
		}
		if p.FilesChanged != nil {
			n += *p.FilesChanged
		}
		return n, true
	}
	if p.TotalFilesProcessed != nil {
		return *p.TotalFilesProcessed, true
	}
	return 0, false
}

// planName is the plan id, or "" for a repo-scoped operation. Prune, check and
// forget run against a repo with no plan of their own, and Backrest fills the
// hook's .Plan.Id with its "_system_" sentinel rather than leaving it empty.
func (p *backrestPayload) planName() string {
	if p.Plan == planSystem {
		return ""
	}
	return p.Plan
}

// elapsed returns how long the operation took. restic's own total_duration
// wins because it times the backup itself; duration_ms is Backrest's wall clock
// for the whole task and also covers hook overhead.
func (p *backrestPayload) elapsed() (time.Duration, bool) {
	if p.TotalDuration != nil && *p.TotalDuration > 0 {
		return time.Duration(*p.TotalDuration * float64(time.Second)), true
	}
	if p.DurationMs > 0 {
		return time.Duration(p.DurationMs) * time.Millisecond, true
	}
	return 0, false
}
