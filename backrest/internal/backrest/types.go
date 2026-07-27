package backrest

import (
	"encoding/json"
	"fmt"
	"time"
)

// Operation status values. Backrest renders the enum by name, and the numbers
// behind those names are not contiguous (ERROR is 4, WARNING is 7), so the name
// is the only thing worth decoding.
const (
	StatusUnknown         = "STATUS_UNKNOWN"
	StatusPending         = "STATUS_PENDING"
	StatusInProgress      = "STATUS_INPROGRESS"
	StatusSuccess         = "STATUS_SUCCESS"
	StatusWarning         = "STATUS_WARNING"
	StatusError           = "STATUS_ERROR"
	StatusSystemCancelled = "STATUS_SYSTEM_CANCELLED"
	StatusUserCancelled   = "STATUS_USER_CANCELLED"
)

// PlanSystem is Backrest's stand-in plan id on a repo-scoped task (prune,
// check, forget). It is a sentinel, not a plan the user named.
const PlanSystem = "_system_"

// Int64 decodes a protobuf int64. The canonical proto-JSON mapping renders
// every 64-bit integer as a *quoted string* to survive JSON's float53 limit, so
// "846209740490" is the normal case and a bare number is the exception. Both
// are accepted: the API is the only producer today, but a fixture written by
// hand should not be a decode failure.
type Int64 int64

func (v *Int64) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			return nil
		}
		var n int64
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
			return fmt.Errorf("parsing int64 %q: %w", s, err)
		}
		*v = Int64(n)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*v = Int64(n)
	return nil
}

func (v Int64) Int64() int64 { return int64(v) }

// OperationList is the GetOperations response body.
type OperationList struct {
	Operations []Operation `json:"operations"`
}

// Operation is one row of Backrest's oplog. Only the fields this bridge reads
// are declared; the wire message carries more.
//
// Every field is optional on the wire. proto-JSON omits zero values entirely,
// so an operation at 0% arrives with no percentDone, no bytesDone and no
// filesDone key at all - absent has to mean zero, never malformed.
type Operation struct {
	ID             Int64  `json:"id"`
	FlowID         Int64  `json:"flowId"`
	Modno          Int64  `json:"modno"`
	InstanceID     string `json:"instanceId"`
	RepoID         string `json:"repoId"`
	RepoGUID       string `json:"repoGuid"`
	PlanID         string `json:"planId"`
	SnapshotID     string `json:"snapshotId"`
	Status         string `json:"status"`
	UnixTimeStart  Int64  `json:"unixTimeStartMs"`
	UnixTimeEnd    Int64  `json:"unixTimeEndMs"`
	DisplayMessage string `json:"displayMessage"`
	Logref         string `json:"logref"`

	// The `op` oneof. At most one is non-nil; all of them are nil for an
	// operation type this bridge does not model.
	Backup *OperationBackup `json:"operationBackup"`
	Prune  *OperationPrune  `json:"operationPrune"`
	Check  *OperationCheck  `json:"operationCheck"`
}

// OperationBackup is present but empty when a backup fails before restic emits
// its first status line (a repo lock timeout, say), so LastStatus being nil is
// an ordinary outcome rather than a decode problem.
type OperationBackup struct {
	LastStatus *BackupProgressEntry  `json:"lastStatus"`
	Errors     []BackupProgressError `json:"errors"`
	DryRun     bool                  `json:"dryRun"`
}

type OperationPrune struct {
	OutputLogref string `json:"outputLogref"`
}

type OperationCheck struct {
	OutputLogref string `json:"outputLogref"`
}

// BackupProgressEntry is a oneof, and the switch is the single most important
// thing to get right about this API: a running backup carries Status, and the
// instant it finishes that key disappears from the JSON and Summary takes its
// place. Code that only looks for Status reads every completed backup as
// malformed.
type BackupProgressEntry struct {
	Status  *BackupProgressStatus  `json:"status"`
	Summary *BackupProgressSummary `json:"summary"`
}

// BackupProgressStatus is restic's live counter, republished by Backrest on
// each status line. There is no seconds-remaining field anywhere in the
// protocol; an ETA has to be derived from how fast BytesDone moves.
type BackupProgressStatus struct {
	PercentDone float64  `json:"percentDone"`
	TotalFiles  Int64    `json:"totalFiles"`
	TotalBytes  Int64    `json:"totalBytes"`
	FilesDone   Int64    `json:"filesDone"`
	BytesDone   Int64    `json:"bytesDone"`
	CurrentFile []string `json:"currentFile"`
}

// BackupProgressSummary is restic's closing report. Doubles stay bare numbers
// in proto-JSON, so TotalDuration is a float64 while its int64 neighbours are
// strings.
type BackupProgressSummary struct {
	FilesNew            Int64   `json:"filesNew"`
	FilesChanged        Int64   `json:"filesChanged"`
	FilesUnmodified     Int64   `json:"filesUnmodified"`
	DirsNew             Int64   `json:"dirsNew"`
	DirsChanged         Int64   `json:"dirsChanged"`
	DirsUnmodified      Int64   `json:"dirsUnmodified"`
	DataBlobs           Int64   `json:"dataBlobs"`
	TreeBlobs           Int64   `json:"treeBlobs"`
	DataAdded           Int64   `json:"dataAdded"`
	TotalFilesProcessed Int64   `json:"totalFilesProcessed"`
	TotalBytesProcessed Int64   `json:"totalBytesProcessed"`
	TotalDuration       float64 `json:"totalDuration"`
	SnapshotID          string  `json:"snapshotId"`
}

// BackupProgressError is one file restic could not read. Backrest keeps these
// separate from the operation's own failure: a backup can finish with a
// snapshot and still list a dozen of them.
type BackupProgressError struct {
	Item    string `json:"item"`
	During  string `json:"during"`
	Message string `json:"message"`
}

// Kind names the operation type this bridge cares about. Anything else - index,
// forget, stats, hook runs - reports KindOther and is skipped.
type Kind string

const (
	KindBackup Kind = "backup"
	KindPrune  Kind = "prune"
	KindCheck  Kind = "check"
	KindOther  Kind = "other"
)

func (o *Operation) Kind() Kind {
	switch {
	case o.Backup != nil:
		return KindBackup
	case o.Prune != nil:
		return KindPrune
	case o.Check != nil:
		return KindCheck
	}
	return KindOther
}

// Running reports whether the operation is still in flight.
func (o *Operation) Running() bool { return o.Status == StatusInProgress }

// Failed reports whether the operation ended badly. Cancellations count: from
// the activity's point of view a cancelled backup is a backup that did not
// happen, and rendering it green would be a lie.
func (o *Operation) Failed() bool {
	switch o.Status {
	case StatusError, StatusSystemCancelled, StatusUserCancelled:
		return true
	}
	return false
}

// Terminal reports whether the operation will not change again.
func (o *Operation) Terminal() bool {
	switch o.Status {
	case StatusSuccess, StatusWarning, StatusError, StatusSystemCancelled, StatusUserCancelled:
		return true
	}
	return false
}

// PlanName is the plan id, or "" for a repo-scoped operation. Prune and check
// run against a repo with no plan of their own and Backrest fills PlanID with
// its sentinel rather than leaving it empty.
func (o *Operation) PlanName() string {
	if o.PlanID == PlanSystem {
		return ""
	}
	return o.PlanID
}

// OutputLogref is the command-output log for a prune or check, or "" when the
// operation has no such log (backups keep their output in the task log
// instead).
func (o *Operation) OutputLogref() string {
	switch {
	case o.Prune != nil:
		return o.Prune.OutputLogref
	case o.Check != nil:
		return o.Check.OutputLogref
	}
	return ""
}

// Progress reports how far a running backup has got, in 0..1.
//
// BytesDone/TotalBytes is preferred over restic's own PercentDone because it is
// the same ratio at full float64 precision, and because it degrades honestly:
// when TotalBytes is still zero (restic scans before it transfers) there is no
// progress to report yet, which is different from reporting zero.
func (o *Operation) Progress() (float64, bool) {
	st := o.BackupStatus()
	if st == nil {
		return 0, false
	}
	if st.TotalBytes > 0 {
		p := float64(st.BytesDone) / float64(st.TotalBytes)
		return clamp01(p), true
	}
	if st.PercentDone > 0 {
		return clamp01(st.PercentDone), true
	}
	return 0, false
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}

// BackupStatus returns the live counter, or nil once the backup has finished
// (or before restic emitted its first status line).
func (o *Operation) BackupStatus() *BackupProgressStatus {
	if o.Backup == nil || o.Backup.LastStatus == nil {
		return nil
	}
	return o.Backup.LastStatus.Status
}

// BackupSummary returns restic's closing report, or nil while the backup is
// still running.
func (o *Operation) BackupSummary() *BackupProgressSummary {
	if o.Backup == nil || o.Backup.LastStatus == nil {
		return nil
	}
	return o.Backup.LastStatus.Summary
}

// Elapsed returns how long the operation took. restic's own TotalDuration wins
// when present because it times the transfer itself; the unix timestamps also
// cover Backrest's task setup and its hooks.
func (o *Operation) Elapsed() (time.Duration, bool) {
	if s := o.BackupSummary(); s != nil && s.TotalDuration > 0 {
		return time.Duration(s.TotalDuration * float64(time.Second)), true
	}
	if o.UnixTimeEnd > 0 && o.UnixTimeStart > 0 && o.UnixTimeEnd > o.UnixTimeStart {
		return time.Duration(o.UnixTimeEnd-o.UnixTimeStart) * time.Millisecond, true
	}
	return 0, false
}

// FilesTouched returns how many files the snapshot actually backed up: new plus
// changed. TotalFilesProcessed is not that number - restic counts every file it
// walked, so on a mostly-unchanged tree it reads in the tens of thousands next
// to a few MB added.
func (s *BackupProgressSummary) FilesTouched() int64 {
	return s.FilesNew.Int64() + s.FilesChanged.Int64()
}
