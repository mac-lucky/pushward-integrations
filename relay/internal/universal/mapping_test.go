package universal

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func shapesOf(t *testing.T, body string) []ShapeField {
	t.Helper()
	fields, _ := flatten(t, body)
	return ShapesOf(fields)
}

func TestNewMapping(t *testing.T) {
	s := shapesOf(t, `{
		"status": "Not classified",
		"state": "RESOLVED",
		"title": "Disk full",
		"note": "an ordinary sentence",
		"job": {"progress": 0.4, "percent_done": 0.4, "done": 42, "count": 1, "done_ratio": 1, "left": -3}
	}`)
	p := Proposal{
		Title: "title", Severity: "status", Lifecycle: "state", Progress: "job.progress", Kind: KindAlert,
		SeverityValues:  map[string]string{"Not classified": SeverityInfo, "made_up": "loud"},
		LifecycleValues: map[string]string{"RESOLVED": LifecycleEnded},
	}
	m := NewMapping(p, s)
	want := Mapping{
		V:               MappingVersion,
		Kind:            KindAlert,
		Paths:           map[Role]string{RoleTitle: "title", RoleSeverity: "status", RoleLifecycle: "state", RoleProgress: "job.progress"},
		Classes:         map[Role]ValueClass{RoleTitle: ClassText, RoleSeverity: ClassText, RoleLifecycle: ClassEnum, RoleProgress: ClassNumber},
		ProgressScale:   ScaleRatio,
		SeverityValues:  map[string]string{"not_classified": SeverityInfo},
		LifecycleValues: map[string]string{"resolved": LifecycleEnded},
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("NewMapping =\n%+v\nwant\n%+v", m, want)
	}
	if err := m.Validate(NewShape(s, false)); err != nil {
		t.Errorf("Validate: %v", err)
	}

	// A text field only keeps table keys that are vocabulary words.
	p = Proposal{Title: "title", Severity: "note", Kind: KindAlert, SeverityValues: map[string]string{"an ordinary sentence": SeverityInfo}}
	if m := NewMapping(p, s); m.SeverityValues != nil {
		t.Errorf("free text became a table key: %v", m.SeverityValues)
	}

	// Tables keep one entry per normalized key: the first in sorted order.
	p = Proposal{Lifecycle: "state", Kind: KindAlert, LifecycleValues: map[string]string{"resolved": LifecycleOngoing, "RESOLVED": LifecycleEnded}}
	if m := NewMapping(p, s); !reflect.DeepEqual(m.LifecycleValues, map[string]string{"resolved": LifecycleEnded}) {
		t.Errorf("colliding keys gave %v", m.LifecycleValues)
	}

	// A sample of 0 or 1 says nothing about the scale: percent unless the key
	// says ratio.
	for path, scale := range map[string]string{
		"job.progress": ScaleRatio, "job.percent_done": ScalePercent, "job.done": ScalePercent,
		"job.count": ScalePercent, "job.done_ratio": ScaleRatio, "job.left": ScaleRatio,
	} {
		if m := NewMapping(Proposal{Progress: path, Kind: KindProgress}, s); m.ProgressScale != scale {
			t.Errorf("progress %s: scale %q, want %q", path, m.ProgressScale, scale)
		}
	}
}

func TestMappingJSON(t *testing.T) {
	m := Mapping{V: 1, Kind: KindAlert, Paths: map[Role]string{RoleTitle: "title"}, ProgressScale: ScaleRatio}
	b, _ := json.Marshal(m)
	if string(b) != `{"v":1,"k":"alert","p":{"title":"title"},"ps":"ratio"}` {
		t.Errorf("mapping JSON = %s", b)
	}
}

func TestValidate(t *testing.T) {
	s := NewShape(shapesOf(t, `{
		"title": "Disk full",
		"body": "Disk /var on web-1 is full",
		"status": "firing",
		"link": "https://example.com",
		"api_token": "abc",
		"trace": "`+testRandom+`",
		"owner": "anna@example.com",
		"subject": null,
		"load": 0.9,
		"pct": "42"
	}`), false)
	ok := Mapping{
		V: MappingVersion, Kind: KindAlert,
		Paths:           map[Role]string{RoleTitle: "title", RoleBody: "body", RoleURL: "link", RoleLifecycle: "status", RoleProgress: "load"},
		ProgressScale:   ScaleRatio,
		LifecycleValues: map[string]string{"firing": LifecycleOngoing},
	}
	if err := ok.Validate(s); err != nil {
		t.Fatalf("valid mapping: %v", err)
	}
	// A user may pick what the heuristic never proposes: an email, a field
	// empty in this sample, a token that is only secret by its looks.
	also := ok
	also.Paths = map[Role]string{RoleTitle: "owner", RoleBody: "subject", RoleCorrelation: "trace", RoleProgress: "subject"}
	if err := also.Validate(s); err != nil {
		t.Errorf("valid mapping: %v", err)
	}
	tooMany := map[string]string{}
	for i := range MaxTableEntries + 1 {
		tooMany[fmt.Sprint("v", i)] = SeverityInfo
	}
	cases := map[string]func(m *Mapping){
		"version":         func(m *Mapping) { m.V = 2 },
		"kind":            func(m *Mapping) { m.Kind = "banner" },
		"unknown role":    func(m *Mapping) { m.Paths["icon"] = "title" },
		"missing path":    func(m *Mapping) { m.Paths[RoleTitle] = "headline" },
		"title is body":   func(m *Mapping) { m.Paths[RoleBody] = "title" },
		"secret":          func(m *Mapping) { m.Paths[RoleCorrelation] = "api_token" },
		"secret title":    func(m *Mapping) { m.Paths[RoleTitle] = "trace" },
		"url not a url":   func(m *Mapping) { m.Paths[RoleURL] = "title" },
		"progress text":   func(m *Mapping) { m.Paths[RoleProgress] = "status" },
		"progress string": func(m *Mapping) { m.Paths[RoleProgress] = "pct" },
		"raw key":         func(m *Mapping) { m.LifecycleValues["Firing"] = LifecycleOngoing },
		"scale":           func(m *Mapping) { m.ProgressScale = "fraction" },
		"no scale":        func(m *Mapping) { m.ProgressScale = "" },
		"state":           func(m *Mapping) { m.LifecycleValues["firing"] = "paused" },
		"severity state":  func(m *Mapping) { m.SeverityValues = map[string]string{"p1": LifecycleEnded} },
		"long key":        func(m *Mapping) { m.LifecycleValues[strings.Repeat("x", MaxTableKeyRunes+1)] = LifecycleEnded },
		"empty key":       func(m *Mapping) { m.LifecycleValues[""] = LifecycleEnded },
		"too many values": func(m *Mapping) { m.SeverityValues = tooMany },
	}
	for name, change := range cases {
		m := ok
		m.Paths = map[Role]string{}
		for r, p := range ok.Paths {
			m.Paths[r] = p
		}
		m.LifecycleValues = map[string]string{"firing": LifecycleOngoing}
		change(&m)
		if err := m.Validate(s); err == nil {
			t.Errorf("%s: Validate passed", name)
		}
	}
}

// A heuristic proposal is always a valid mapping, so the heuristic can be the
// fallback for anything. TestShapesRoundTrip checks the relay fixtures too.
func TestHeuristicProposalsValidate(t *testing.T) {
	for _, body := range []string{
		`{"status": "firing", "severity": "critical", "alert_id": "a1b2c3d4e5", "title": "Disk almost full",
		  "description": "Disk /var on web-1 is 95% full", "dashboard_url": "https://grafana.example.com/d/abc"}`,
		`{"event_id": "9b1f0c2e7d", "job": {"id": 42, "name": "nightly backup", "state": "running", "progress": 0.4}}`,
		`{"subject": "anna@example.com", "message": "Anna replied", "session": "s3cr3t", "token_id": 7}`,
	} {
		s := shapesOf(t, body)
		m := NewMapping(ProposeShapes(s), s)
		if err := m.Validate(NewShape(s, false)); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
}

func TestNewShapeCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("{")
	for i := range MaxPaths {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"%s_%03d": "value"`, strings.Repeat("k", 100), i)
	}
	b.WriteString("}")
	all := shapesOf(t, b.String())
	last := all[len(all)-1].Path

	s := NewShape(all, false, last)
	if !s.Truncated {
		t.Error("Truncated = false past the size cap")
	}
	raw, _ := json.Marshal(s)
	if len(raw) > MaxShapeBytes {
		t.Errorf("stored shape is %d bytes, cap is %d", len(raw), MaxShapeBytes)
	}
	if len(s.Fields) < 2 || s.Fields[0].Path != all[0].Path || s.Fields[len(s.Fields)-1].Path != last {
		t.Errorf("kept %d fields; want the leading ones plus the kept last one", len(s.Fields))
	}

	small := NewShape(all[:3], true)
	if !small.Truncated || len(small.Fields) != 3 {
		t.Errorf("small shape: truncated=%v fields=%d", small.Truncated, len(small.Fields))
	}
}
