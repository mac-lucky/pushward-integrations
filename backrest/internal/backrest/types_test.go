package backrest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// load reads a fixture. Every file under testdata is a real operation put back
// through the same protojson encoder the API uses, so a decode that works here
// works against the server.
func load(t *testing.T, name string) *Operation {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", name)) // #nosec G304 -- this module's own testdata dir, not user input
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	var op Operation
	if err := json.Unmarshal(raw, &op); err != nil {
		t.Fatalf("decoding fixture %s: %v", name, err)
	}
	return &op
}

// The whole reason for real fixtures: every 64-bit field arrives quoted, and a
// decoder that assumed JSON numbers would fail on all of them.
func TestQuotedInt64Fields(t *testing.T) {
	op := load(t, "backup_inprogress.json")

	if got := op.ID.Int64(); got != 11162 {
		t.Errorf("id = %d, want 11162", got)
	}
	if got := op.UnixTimeStart.Int64(); got != 1782724420436 {
		t.Errorf("unixTimeStartMs = %d, want 1782724420436", got)
	}

	st := op.BackupStatus()
	if st == nil {
		t.Fatal("BackupStatus() = nil, want the live counter")
	}
	if got := st.TotalBytes.Int64(); got != 846209740490 {
		t.Errorf("totalBytes = %d, want 846209740490", got)
	}
	if got := st.BytesDone.Int64(); got != 800995270310 {
		t.Errorf("bytesDone = %d, want 800995270310", got)
	}
	if got := st.FilesDone.Int64(); got != 597 {
		t.Errorf("filesDone = %d, want 597", got)
	}
	// Doubles stay bare numbers even while their int64 neighbours are strings.
	if st.PercentDone < 0.94 || st.PercentDone > 0.95 {
		t.Errorf("percentDone = %v, want ~0.9466", st.PercentDone)
	}
}

func TestInt64AcceptsBareNumber(t *testing.T) {
	var v struct {
		N Int64 `json:"n"`
	}
	if err := json.Unmarshal([]byte(`{"n":42}`), &v); err != nil {
		t.Fatalf("decoding bare number: %v", err)
	}
	if v.N.Int64() != 42 {
		t.Errorf("n = %d, want 42", v.N.Int64())
	}
}

func TestInt64AcceptsNullAndEmpty(t *testing.T) {
	for _, raw := range []string{`{"n":null}`, `{"n":""}`} {
		var v struct {
			N Int64 `json:"n"`
		}
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("decoding %s: %v", raw, err)
		}
		if v.N.Int64() != 0 {
			t.Errorf("%s gave n = %d, want 0", raw, v.N.Int64())
		}
	}
}

// The oneof is the trap that breaks naive decoders: the key a running backup
// carries is gone the moment it finishes.
func TestLastStatusOneofSwitchesAtCompletion(t *testing.T) {
	running := load(t, "backup_inprogress.json")
	if running.BackupStatus() == nil {
		t.Error("running backup: BackupStatus() = nil, want a live counter")
	}
	if running.BackupSummary() != nil {
		t.Error("running backup: BackupSummary() is set, want nil")
	}

	done := load(t, "backup_success.json")
	if done.BackupStatus() != nil {
		t.Error("finished backup: BackupStatus() is set, want nil")
	}
	if done.BackupSummary() == nil {
		t.Error("finished backup: BackupSummary() = nil, want the closing report")
	}
}

// proto-JSON omits zero values entirely, so absent has to read as zero rather
// than as a decode failure. backup_success.json carries no filesNew or dirsNew.
func TestOmittedZeroValuesDecodeAsZero(t *testing.T) {
	op := load(t, "backup_success.json")
	s := op.BackupSummary()
	if s == nil {
		t.Fatal("BackupSummary() = nil")
	}
	if s.FilesNew.Int64() != 0 {
		t.Errorf("filesNew = %d, want 0 (the key is absent)", s.FilesNew.Int64())
	}
	if s.DirsNew.Int64() != 0 {
		t.Errorf("dirsNew = %d, want 0 (the key is absent)", s.DirsNew.Int64())
	}
	// ...while the keys that were present still decode.
	if s.FilesChanged.Int64() != 785 {
		t.Errorf("filesChanged = %d, want 785", s.FilesChanged.Int64())
	}
	if s.FilesTouched() != 785 {
		t.Errorf("FilesTouched() = %d, want 785", s.FilesTouched())
	}
}

// operationBackup present but empty is an ordinary outcome - the backup died
// before restic emitted a status line - and must not panic or error.
func TestEmptyOperationBackupIsNotAnError(t *testing.T) {
	op := load(t, "backup_error_nostatus.json")
	if op.Kind() != KindBackup {
		t.Errorf("Kind() = %q, want backup", op.Kind())
	}
	if op.BackupStatus() != nil {
		t.Error("BackupStatus() is set, want nil")
	}
	if op.BackupSummary() != nil {
		t.Error("BackupSummary() is set, want nil")
	}
	if _, ok := op.Progress(); ok {
		t.Error("Progress() reported a value, want none")
	}
	if !op.Failed() {
		t.Error("Failed() = false, want true")
	}
}

func TestKindDetection(t *testing.T) {
	tests := []struct {
		fixture string
		want    Kind
	}{
		{"backup_inprogress.json", KindBackup},
		{"backup_success.json", KindBackup},
		{"prune_success.json", KindPrune},
		{"check_error.json", KindCheck},
	}
	for _, tc := range tests {
		if got := load(t, tc.fixture).Kind(); got != tc.want {
			t.Errorf("%s: Kind() = %q, want %q", tc.fixture, got, tc.want)
		}
	}
}

func TestStatusPredicates(t *testing.T) {
	tests := []struct {
		fixture           string
		running, failed   bool
		terminal          bool
		describeStatement string
	}{
		{"backup_inprogress.json", true, false, false, "a running backup"},
		{"backup_success.json", false, false, true, "a clean backup"},
		{"backup_error_partial.json", false, true, true, "a backup that died mid-transfer"},
		{"backup_warning_errors.json", false, false, true, "a backup with unreadable files"},
		{"check_error.json", false, true, true, "a failed check"},
	}
	for _, tc := range tests {
		op := load(t, tc.fixture)
		if got := op.Running(); got != tc.running {
			t.Errorf("%s (%s): Running() = %v, want %v", tc.fixture, tc.describeStatement, got, tc.running)
		}
		if got := op.Failed(); got != tc.failed {
			t.Errorf("%s (%s): Failed() = %v, want %v", tc.fixture, tc.describeStatement, got, tc.failed)
		}
		if got := op.Terminal(); got != tc.terminal {
			t.Errorf("%s (%s): Terminal() = %v, want %v", tc.fixture, tc.describeStatement, got, tc.terminal)
		}
	}
}

// A cancelled backup is a backup that did not happen. Rendering it green would
// be a lie, so Failed has to cover both cancellation statuses.
func TestCancellationCountsAsFailure(t *testing.T) {
	for _, status := range []string{StatusSystemCancelled, StatusUserCancelled} {
		op := &Operation{Status: status}
		if !op.Failed() {
			t.Errorf("%s: Failed() = false, want true", status)
		}
		if !op.Terminal() {
			t.Errorf("%s: Terminal() = false, want true", status)
		}
	}
}

func TestProgressPrefersByteRatio(t *testing.T) {
	op := load(t, "backup_inprogress.json")
	got, ok := op.Progress()
	if !ok {
		t.Fatal("Progress() reported no value")
	}
	// 800995270310 / 846209740490
	want := 800995270310.0 / 846209740490.0
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Progress() = %v, want the byte ratio %v", got, want)
	}
}

// While restic is still scanning, TotalBytes is zero. That is "no progress
// yet", which is a different statement from "0% done" and must not render as a
// bar sitting at the far left.
func TestProgressWithoutTotalReportsNothing(t *testing.T) {
	op := &Operation{
		Status: StatusInProgress,
		Backup: &OperationBackup{
			LastStatus: &BackupProgressEntry{
				Status: &BackupProgressStatus{BytesDone: 1024},
			},
		},
	}
	if _, ok := op.Progress(); ok {
		t.Error("Progress() reported a value while restic was still scanning")
	}
}

func TestProgressIsClamped(t *testing.T) {
	op := &Operation{
		Backup: &OperationBackup{
			LastStatus: &BackupProgressEntry{
				Status: &BackupProgressStatus{BytesDone: 300, TotalBytes: 100},
			},
		},
	}
	got, ok := op.Progress()
	if !ok {
		t.Fatal("Progress() reported no value")
	}
	if got != 1.0 {
		t.Errorf("Progress() = %v, want 1 (clamped)", got)
	}
}

// The plan sentinel is Backrest's, not a plan the user named, so it must not
// reach the UI.
func TestSystemPlanRendersAsNoPlan(t *testing.T) {
	op := load(t, "prune_success.json")
	if op.PlanID != PlanSystem {
		t.Fatalf("fixture planId = %q, want the sentinel", op.PlanID)
	}
	if got := op.PlanName(); got != "" {
		t.Errorf("PlanName() = %q, want empty", got)
	}
}

func TestOutputLogref(t *testing.T) {
	if got := load(t, "prune_success.json").OutputLogref(); got != "c-1e03d4bb-9fbc-42ca-bba3-6a99d951cf5f" {
		t.Errorf("prune OutputLogref() = %q", got)
	}
	if got := load(t, "check_error.json").OutputLogref(); got != "c-00bfa525-65ad-42da-b040-7e538d178771" {
		t.Errorf("check OutputLogref() = %q", got)
	}
	// Backups keep their output in the task log instead.
	if got := load(t, "backup_success.json").OutputLogref(); got != "" {
		t.Errorf("backup OutputLogref() = %q, want empty", got)
	}
}

// restic's own duration times the transfer; the unix timestamps also cover
// Backrest's task setup and its hooks, so the two disagree and restic wins.
func TestElapsedPrefersResticDuration(t *testing.T) {
	op := load(t, "backup_success.json")
	got, ok := op.Elapsed()
	if !ok {
		t.Fatal("Elapsed() reported no value")
	}
	want := time.Duration(10.016478984 * float64(time.Second))
	if got != want {
		t.Errorf("Elapsed() = %v, want restic's %v (not the %v wall clock)", got, want,
			time.Duration(op.UnixTimeEnd-op.UnixTimeStart)*time.Millisecond)
	}
}

func TestElapsedFallsBackToWallClock(t *testing.T) {
	op := load(t, "check_error.json")
	got, ok := op.Elapsed()
	if !ok {
		t.Fatal("Elapsed() reported no value")
	}
	want := time.Duration(1782006954468-1782006087379) * time.Millisecond
	if got != want {
		t.Errorf("Elapsed() = %v, want %v", got, want)
	}
}

func TestOperationListDecodes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "operations_list.json"))
	if err != nil {
		t.Fatal(err)
	}
	var list OperationList
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decoding operation list: %v", err)
	}
	if len(list.Operations) != 3 {
		t.Fatalf("got %d operations, want 3", len(list.Operations))
	}
	// The window is mixed: a finished backup, an operation type this bridge
	// does not model, and the one that is actually running.
	kinds := []Kind{KindBackup, KindOther, KindBackup}
	for i, want := range kinds {
		if got := list.Operations[i].Kind(); got != want {
			t.Errorf("operations[%d].Kind() = %q, want %q", i, got, want)
		}
	}
	if !list.Operations[2].Running() {
		t.Error("operations[2] should be the running backup")
	}
}

func TestBackupErrorsDecode(t *testing.T) {
	op := load(t, "backup_warning_errors.json")
	if op.Backup == nil {
		t.Fatal("operationBackup missing")
	}
	if len(op.Backup.Errors) != 2 {
		t.Fatalf("got %d per-file errors, want 2", len(op.Backup.Errors))
	}
	if op.Backup.Errors[0].Item != "/data/live/session.db" {
		t.Errorf("errors[0].item = %q", op.Backup.Errors[0].Item)
	}
	// A backup can list unreadable files and still produce a snapshot.
	if op.BackupSummary() == nil {
		t.Error("BackupSummary() = nil, want the closing report alongside the errors")
	}
}
