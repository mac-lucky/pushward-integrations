package universal

import (
	"reflect"
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
