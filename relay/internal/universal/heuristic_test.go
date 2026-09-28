package universal

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"
)

func TestProposeAlert(t *testing.T) {
	fields, _ := flatten(t, `{
		"status": "firing",
		"severity": "critical",
		"alert_id": "a1b2c3d4e5",
		"title": "Disk almost full",
		"description": "Disk /var on web-1 is 95% full and still growing",
		"dashboard_url": "https://grafana.example.com/d/abc",
		"fired_at": "2026-09-26T10:00:00Z"
	}`)
	got := Propose(fields)
	want := Proposal{
		Title:           "title",
		Body:            "description",
		URL:             "dashboard_url",
		Correlation:     "alert_id",
		Severity:        "severity",
		Lifecycle:       "status",
		Kind:            KindAlert,
		SeverityValues:  map[string]string{"critical": SeverityCritical},
		LifecycleValues: map[string]string{"firing": LifecycleOngoing},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Propose =\n%+v\nwant\n%+v", got, want)
	}
}

func TestProposeProgress(t *testing.T) {
	fields, _ := flatten(t, `{
		"event_id": "9b1f0c2e7d",
		"job": {"id": 42, "name": "nightly backup", "state": "running", "progress": 0.4, "bytes": 1024},
		"repository": {"id": 7, "name": "infra"}
	}`)
	got := Propose(fields)
	if got.Kind != KindProgress {
		t.Errorf("kind = %s, want progress", got.Kind)
	}
	checks := map[Role]string{
		RoleTitle:       "job.name",
		RoleProgress:    "job.progress",
		RoleLifecycle:   "job.state",
		RoleCorrelation: "job.id",
		RoleSeverity:    "",
	}
	for r, want := range checks {
		if p := got.Path(r); p != want {
			t.Errorf("%s = %q, want %q", r, p, want)
		}
	}
	if got.LifecycleValues["running"] != LifecycleOngoing {
		t.Errorf("lifecycle values = %v", got.LifecycleValues)
	}
}

func TestProposeNotification(t *testing.T) {
	fields, _ := flatten(t, `{
		"subject": "New comment",
		"message": "Anna replied to your post about the garden",
		"avatar_url": "https://example.com/a.png",
		"link": "https://example.com/p/1",
		"read": false
	}`)
	got := Propose(fields)
	want := Proposal{Title: "subject", Body: "message", URL: "link", Kind: KindNotification}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Propose = %+v, want %+v", got, want)
	}
}

func TestProposeNothingToMap(t *testing.T) {
	fields, _ := flatten(t, `{"a": true, "b": null, "c": []}`)
	got := Propose(fields)
	if !reflect.DeepEqual(got, Proposal{Kind: KindNotification}) {
		t.Errorf("Propose = %+v, want an empty notification mapping", got)
	}
}

func TestRank(t *testing.T) {
	fields, _ := flatten(t, `{"enabled": true, "name": "backup", "title": "Nightly backup", "count": 3}`)
	c := Rank(fields, RoleTitle)
	if len(c) != 2 || c[0].Path != "title" || c[1].Path != "name" {
		t.Errorf("Rank(title) = %+v, want title then name, no bool or number", c)
	}
	if !reflect.DeepEqual(c, Rank(fields, RoleTitle)) {
		t.Error("Rank is not deterministic")
	}
	if c := Rank(fields, RoleURL); len(c) != 0 {
		t.Errorf("Rank(url) = %+v, want nothing without a URL value", c)
	}
}

func TestTokens(t *testing.T) {
	cases := map[string][]string{
		"html_url":      {"html", "url"},
		"htmlUrl":       {"html", "url"},
		"commonLabels":  {"common", "labels"},
		"alerts[]":      {"alerts"},
		"X-Event-Type":  {"x", "event", "type"},
		"run_id":        {"run", "id"},
		"percentDone2x": {"percent", "done2x"},
	}
	for in, want := range cases {
		if got := tokens(in); !reflect.DeepEqual(got, want) {
			t.Errorf("tokens(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestValueTail(t *testing.T) {
	cases := map[string]string{
		"alert.resolved":         "resolved",
		"sync-failed":            "failed",
		"CONDITION_SNAPSHOT_END": "end",
		"media.play":             "play",
		"resolved":               "",
		"Backup complete":        "complete",
		"Not Resolved":           "",
		"not-ready":              "",
		"No Error":               "",
		"issue.no_error":         "",
		"non-critical":           "",
		"un-acknowledged":        "",
		"Not yet resolved":       "",
		"never completed":        "",
		"job.never.finished":     "",
	}
	for in, want := range cases {
		if got := valueTail(in); got != want {
			t.Errorf("valueTail(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"Not Resolved": "", "not-ready": "", "No Error": "", "alert.resolved": LifecycleEnded} {
		if got := lifecycleOf(in); got != want {
			t.Errorf("lifecycleOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHeuristicGolden compares the heuristic, fixture by fixture, with what it
// proposed and ranked before it moved onto shapes (testdata/
// heuristic_golden.json, made from commit 0ce0d87). Two changes are allowed:
// value-table keys are normalized (normValue of the old key, or its last
// word), and Rank leaves out fields a role cannot take, so each old top 8,
// less those fields, has to be a prefix of today's list.
func TestHeuristicGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/heuristic_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]struct {
		Proposal   Proposal             `json:"proposal"`
		Candidates map[Role][]Candidate `json:"candidates"`
	}
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatal(err)
	}
	// Fixtures added since have no golden entry; every golden one must still
	// be there.
	fixtures := relayFixtures(t)
	for name, g := range golden {
		fields, ok := fixtures[name]
		if !ok {
			t.Errorf("%s: in the golden file but gone from relay/testdata", name)
			continue
		}
		got := Propose(fields)
		if !sameTable(got.SeverityValues, g.Proposal.SeverityValues) || !sameTable(got.LifecycleValues, g.Proposal.LifecycleValues) {
			t.Errorf("%s: tables %v %v, golden %v %v", name, got.SeverityValues, got.LifecycleValues,
				g.Proposal.SeverityValues, g.Proposal.LifecycleValues)
		}
		got.SeverityValues, got.LifecycleValues = nil, nil
		want := g.Proposal
		want.SeverityValues, want.LifecycleValues = nil, nil
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Propose =\n%+v\ngolden\n%+v", name, got, want)
		}

		shapes := ShapesOf(fields)
		byPath := make(map[string]*ShapeField, len(shapes))
		for i := range shapes {
			byPath[shapes[i].Path] = &shapes[i]
		}
		for _, r := range Roles {
			old := g.Candidates[r]
			var kept []Candidate
			for _, c := range old {
				if f := byPath[c.Path]; f != nil && rankable(r, f) {
					kept = append(kept, c)
				}
			}
			cur := Rank(fields, r)
			prefix := len(cur) >= len(kept) && slices.Equal(cur[:len(kept)], kept)
			// A golden list shorter than 8 was the whole list.
			if !prefix || len(old) < 8 && len(cur) != len(kept) {
				t.Errorf("%s: Rank(%s) =\n%v\ngolden, less what %s cannot take:\n%v", name, r, cur[:min(len(cur), 8)], r, kept)
			}
		}
	}
}

func sameTable(got, golden map[string]string) bool {
	if len(got) != len(golden) {
		return false
	}
	for raw, state := range golden {
		if got[normValue(raw)] != state && (valueTail(raw) == "" || got[valueTail(raw)] != state) {
			return false
		}
	}
	return true
}
