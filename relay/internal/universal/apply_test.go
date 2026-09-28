package universal

import (
	"testing"

	"github.com/mac-lucky/pushward-integrations/shared/text"
)

func TestApply(t *testing.T) {
	m := Mapping{
		V: MappingVersion, Kind: KindAlert,
		Paths: map[Role]string{
			RoleTitle: "title", RoleBody: "description", RoleURL: "link", RoleCorrelation: "alert_id",
			RoleSeverity: "severity", RoleLifecycle: "status",
		},
		SeverityValues:  map[string]string{"sev_a": SeverityCritical},
		LifecycleValues: map[string]string{"closed_out": LifecycleEnded},
	}
	fields, _ := flatten(t, `{
		"title": "  Disk almost full ",
		"description": "Disk /var on web-1 is 95% full",
		"link": "https://grafana.example.com/d/abc",
		"alert_id": "a1b2c3",
		"severity": "Sev-A",
		"status": "Closed Out"
	}`)
	ev := Apply(m, fields, "grafana")
	want := Event{
		Kind: KindAlert, Title: "Disk almost full", Body: "Disk /var on web-1 is 95% full",
		URL: "https://grafana.example.com/d/abc", CorrelationKey: text.HashHex("a1b2c3", 16),
		Severity: SeverityCritical, SevFrom: FromTable,
		Lifecycle: LifecycleEnded, LifecycleRaw: "Closed Out", LcFrom: FromTable,
	}
	if ev != want {
		t.Errorf("Apply =\n%+v\nwant\n%+v", ev, want)
	}
}

func TestApplyValueFallbacks(t *testing.T) {
	m := Mapping{
		V: MappingVersion, Kind: KindAlert,
		Paths:           map[Role]string{RoleSeverity: "severity", RoleLifecycle: "status"},
		LifecycleValues: map[string]string{"resolved": LifecycleEnded},
	}
	cases := []struct {
		body           string
		sev, sevFrom   string
		life, lifeFrom string
	}{
		// The proposal keyed "alert.resolved" by its last word.
		{`{"severity": "warning", "status": "alert.resolved"}`, SeverityWarning, FromHeuristic, LifecycleEnded, FromTable},
		{`{"severity": "HIGH", "status": "Firing"}`, SeverityCritical, FromHeuristic, LifecycleOngoing, FromHeuristic},
		{`{"severity": "Not classified", "status": "done"}`, SeverityInfo, FromHeuristic, LifecycleEnded, FromHeuristic},
		{`{"severity": "3", "status": "sync-failed"}`, SeverityWarning, FromDefault, LifecycleEnded, FromHeuristic},
		// Unknown and negated values never end a card.
		{`{"severity": "whatever", "status": "flapping"}`, SeverityWarning, FromDefault, LifecycleOngoing, FromDefault},
		{`{"severity": "non-critical", "status": "Not Resolved"}`, SeverityWarning, FromDefault, LifecycleOngoing, FromDefault},
		{`{"severity": "no_error", "status": "not-ready"}`, SeverityWarning, FromDefault, LifecycleOngoing, FromDefault},
		{`{"severity": "none", "status": "No Error"}`, SeverityInfo, FromHeuristic, LifecycleOngoing, FromDefault},
		{`{}`, SeverityWarning, FromDefault, LifecycleOngoing, FromDefault},
	}
	for _, c := range cases {
		fields, _ := flatten(t, c.body)
		ev := Apply(m, fields, "src")
		if ev.Severity != c.sev || ev.SevFrom != c.sevFrom || ev.Lifecycle != c.life || ev.LcFrom != c.lifeFrom {
			t.Errorf("%s: severity %s (%s), lifecycle %s (%s); want %s (%s), %s (%s)",
				c.body, ev.Severity, ev.SevFrom, ev.Lifecycle, ev.LcFrom, c.sev, c.sevFrom, c.life, c.lifeFrom)
		}
	}

	m.Kind = KindNotification
	fields, _ := flatten(t, `{}`)
	if ev := Apply(m, fields, "src"); ev.Severity != SeverityInfo {
		t.Errorf("notification default severity = %s, want info", ev.Severity)
	}
}

func TestApplyProgress(t *testing.T) {
	cases := []struct {
		scale, value string
		want         float64 // -1 for nil
	}{
		{ScaleRatio, "0.4", 0.4},
		{ScaleRatio, "1", 1},
		{ScalePercent, "40", 0.4},
		{ScalePercent, "100", 1},
		// A ratio mapping made from a sample of 0 or 1 still reads 45.
		{ScaleRatio, "45", 0.45},
		{ScaleRatio, "140", -1},
		{ScalePercent, "0.5", 0.005},
		{ScalePercent, "140", -1},
		{ScalePercent, "-5", -1},
		{ScaleRatio, `"NaN"`, -1},
		{ScaleRatio, `"0.25"`, 0.25},
	}
	for _, c := range cases {
		m := Mapping{V: MappingVersion, Kind: KindProgress, Paths: map[Role]string{RoleProgress: "p"}, ProgressScale: c.scale}
		fields, _ := flatten(t, `{"p": `+c.value+`}`)
		ev := Apply(m, fields, "")
		switch {
		case c.want < 0 && ev.Progress != nil:
			t.Errorf("%s %s: progress %v, want nil", c.scale, c.value, *ev.Progress)
		case c.want >= 0 && (ev.Progress == nil || *ev.Progress != c.want):
			t.Errorf("%s %s: progress %v, want %v", c.scale, c.value, ev.Progress, c.want)
		}
	}
}

func TestApplyDefaults(t *testing.T) {
	m := Mapping{
		V: MappingVersion, Kind: "banner",
		Paths: map[Role]string{RoleTitle: "api_token", RoleBody: "body", RoleURL: "link", RoleLifecycle: "status"},
	}
	cases := []struct {
		body, source, title, text, url string
	}{
		{`{"api_token": "hlk_abcd1234", "link": "javascript:alert(1)", "status": "Running"}`, "backup", "backup", "Running", ""},
		{`{"link": "ftp://example.com/x"}`, "backup", "backup", "Event from backup", ""},
		{`{"link": "http://example.com/x"}`, "", "Webhook", "Event", "http://example.com/x"},
	}
	for _, c := range cases {
		fields, _ := flatten(t, c.body)
		ev := Apply(m, fields, c.source)
		if ev.Title != c.title || ev.Body != c.text || ev.URL != c.url || ev.Kind != KindNotification {
			t.Errorf("%s: title %q body %q url %q kind %s; want %q %q %q notification",
				c.body, ev.Title, ev.Body, ev.URL, ev.Kind, c.title, c.text, c.url)
		}
	}
}

// A value that only its looks make secret still correlates, but only as a
// hash; a field whose key names a credential is used for nothing.
func TestApplySecrets(t *testing.T) {
	m := Mapping{
		V: MappingVersion, Kind: KindAlert,
		Paths: map[Role]string{RoleTitle: "trace", RoleCorrelation: "trace", RoleBody: "api_token"},
	}
	fields, _ := flatten(t, `{"trace": "`+testRandom+`", "api_token": "hunter22"}`)
	ev := Apply(m, fields, "ci")
	if ev.CorrelationKey != text.HashHex(testRandom, 16) || ev.Title != "ci" || ev.Body != "Event from ci" {
		t.Errorf("Apply = %+v, want the token as correlation only", ev)
	}
	m.Paths[RoleCorrelation] = "api_token"
	if ev := Apply(m, fields, "ci"); ev.CorrelationKey != "" {
		t.Errorf("correlation key = %q from a secret key", ev.CorrelationKey)
	}
}
