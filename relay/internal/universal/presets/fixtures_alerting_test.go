package presets

import "github.com/mac-lucky/pushward-integrations/relay/internal/universal"

func init() {
	addFixtures(map[string]want{
		"alertmanager_firing.json": {
			preset: "alertmanager", title: "HighMemoryUsage",
			body: "Memory on web-2 above 90% for 10 minutes",
			url:  "http://prometheus.lan:9090/graph?g0.expr=mem_used_ratio",
			corr: `{}/{severity="warning"}:{alertname="HighMemoryUsage"}`,
			sev:  universal.SeverityWarning, life: universal.LifecycleOngoing,
		},
		"alertmanager_resolved.json": {
			preset: "alertmanager", title: "HighMemoryUsage",
			body: "Memory on web-2 above 90% for 10 minutes",
			url:  "http://prometheus.lan:9090/graph?g0.expr=mem_used_ratio",
			corr: `{}/{severity="warning"}:{alertname="HighMemoryUsage"}`,
			sev:  universal.SeverityWarning, life: universal.LifecycleEnded,
		},
		"pagerduty-incident_triggered.json": {
			preset: "pagerduty-incident", title: "Checkout latency above 2s", body: "Checkout API",
			url:  "https://example.pagerduty.com/incidents/Q3EXAMPLEINC01",
			corr: "Q3EXAMPLEINC01", sev: universal.SeverityCritical, life: universal.LifecycleOngoing,
		},
		"pagerduty-incident_resolved.json": {
			preset: "pagerduty-incident", title: "Checkout latency above 2s", body: "Checkout API",
			url:  "https://example.pagerduty.com/incidents/Q3EXAMPLEINC01",
			corr: "Q3EXAMPLEINC01", sev: universal.SeverityCritical, life: universal.LifecycleEnded,
		},
	})
}
