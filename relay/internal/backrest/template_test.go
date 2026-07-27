package backrest

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"text/template"
	"time"
)

// docTemplate returns the Backrest hook template exactly as documented in
// relay/README.md, so the tests below exercise the bytes users actually paste
// rather than a copy that can drift from them.
func docTemplate(t *testing.T) string {
	t.Helper()
	readme := readRelayREADME(t)
	// Other providers document their own {"event":...} templates, so scope the
	// search to the Backrest section before looking for the fenced block.
	section := regexp.MustCompile(`(?s)\n### Backrest\n(.*?)\n### `).FindStringSubmatch(readme)
	if section == nil {
		t.Fatal("relay/README.md has no ### Backrest section")
	}
	m := regexp.MustCompile("(?s)```\n(\\{\"event\":.*?)\n```").FindStringSubmatch(section[1])
	if m == nil {
		t.Fatal("the Backrest section has no fenced hook-template block")
	}
	return m[1]
}

// legacyErrorFragment is how the template used to interpolate the error: raw,
// inside a JSON string literal. Backrest renders hook templates with
// text/template, which does not escape, so any quote or newline in a restic
// error produced a body the relay could not parse.
const legacyErrorFragment = `"error":"{{ .Error }}"`

// stubStats mirrors the fields of restic.BackupProgressEntry that the template
// reads (Backrest v1.14.1, pkg/restic/outputs.go).
type stubStats struct {
	FilesNew            int64
	FilesChanged        int64
	FilesUnmodified     int64
	DataAdded           int64
	TotalFilesProcessed int64
	TotalBytesProcessed int64
	TotalDuration       float64
}

type stubID struct{ Id string } //nolint:revive,stylecheck // mirrors the protobuf field name the template uses

// stubVars mirrors the fields of Backrest's HookVars that the template reads,
// including the JsonMarshal helper it exposes
// (v1.14.1, internal/orchestrator/tasks/hookvars.go). Close enough to render:
// Event is really a v1.Hook_Condition, and text/template prints its String().
//
// Executing against a stub catches a field the template asks for and Backrest
// does not have, which is the failure that matters, since a render error kills
// every condition rather than one. It cannot catch the stub itself drifting, so
// re-check it against hookvars.go when bumping the documented Backrest version.
type stubVars struct {
	Task          string
	Event         string
	Repo          *stubID
	Plan          *stubID
	SnapshotId    string //nolint:revive,stylecheck // mirrors Backrest's field name
	SnapshotStats *stubStats
	Duration      time.Duration
	Error         string
}

func (v stubVars) JsonMarshal(s any) string { //nolint:revive,stylecheck // mirrors Backrest's helper name
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(b)
}

func render(t *testing.T, tmpl string, vars stubVars) string {
	t.Helper()
	parsed, err := template.New("hook").Parse(tmpl)
	if err != nil {
		t.Fatalf("parsing template: %v", err)
	}
	var sb strings.Builder
	if err := parsed.Execute(&sb, vars); err != nil {
		t.Fatalf("executing template: %v", err)
	}
	return sb.String()
}

// nastyError is representative, not contrived: Backrest quotes the command it
// ran and the repo URI, and restic writes multi-line failures.
const nastyError = "command \"/bin/restic backup --json /backup/photos\" failed: exit status 1\nFatal: unable to open repo at \"sftp://backup\""

func baseVars() stubVars {
	return stubVars{
		Task:       `backup for plan "daily-backup"`,
		Event:      condSnapshotSuccess,
		Repo:       &stubID{Id: "local-repo"},
		Plan:       &stubID{Id: "daily-backup"},
		SnapshotId: "abc123def",
		Duration:   45 * time.Second,
	}
}

func TestDocTemplateSurvivesQuotedError(t *testing.T) {
	vars := baseVars()
	vars.Event = condAnyError
	vars.Error = nastyError

	out := render(t, docTemplate(t), vars)
	if !json.Valid([]byte(out)) {
		t.Fatalf("documented template emitted invalid JSON: %s", out)
	}

	var p backrestPayload
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("decoding rendered template: %v\n%s", err, out)
	}
	if p.Error != nastyError {
		t.Errorf("error text did not round-trip:\n got %q\nwant %q", p.Error, nastyError)
	}
	if p.Event != condAnyError {
		t.Errorf("expected event %q, got %q", condAnyError, p.Event)
	}
	// No snapshot summary on a catch-all error, so the stats stay absent rather
	// than decoding as zeroes.
	if p.DataAdded != nil {
		t.Errorf("expected data_added absent without SnapshotStats, got %v", *p.DataAdded)
	}
}

func TestDocTemplateCarriesFullSummary(t *testing.T) {
	vars := baseVars()
	vars.SnapshotStats = &stubStats{
		FilesNew: 334, FilesChanged: 14, FilesUnmodified: 65007,
		DataAdded: 85082131721, TotalFilesProcessed: 65355,
		TotalBytesProcessed: 1195256287571, TotalDuration: 906.366048946,
	}

	out := render(t, docTemplate(t), vars)
	var p backrestPayload
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("decoding rendered template: %v\n%s", err, out)
	}

	if p.DataAdded == nil || *p.DataAdded != 85082131721 {
		t.Errorf("data_added did not round-trip: %v", p.DataAdded)
	}
	if p.TotalFilesProcessed == nil || *p.TotalFilesProcessed != 65355 {
		t.Errorf("total_files_processed did not round-trip: %v", p.TotalFilesProcessed)
	}
	if p.TotalDuration == nil || *p.TotalDuration != 906.366048946 {
		t.Errorf("total_duration did not round-trip: %v", p.TotalDuration)
	}
	// The task name is quoted by Backrest itself, so it exercises escaping too.
	if p.Task != `backup for plan "daily-backup"` {
		t.Errorf("task did not round-trip: %q", p.Task)
	}
	if p.DurationMs != 45000 {
		t.Errorf("expected duration_ms 45000, got %d", p.DurationMs)
	}
	if got := scanDetail(&p); got != "1.1 TB scanned · 65007 unchanged" {
		t.Errorf("unexpected scan detail: %q", got)
	}
}

// The unescaped form silently loses every failure notification, so it must not
// survive anywhere in the docs people copy from.
func TestREADMEDropsLegacyTemplate(t *testing.T) {
	if strings.Contains(readRelayREADME(t), legacyErrorFragment) {
		t.Errorf("relay/README.md still documents the unescaped %s form", legacyErrorFragment)
	}
}

func readRelayREADME(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("reading relay README: %v", err)
	}
	return string(b)
}
