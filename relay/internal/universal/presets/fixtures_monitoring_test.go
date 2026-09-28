package presets

import "github.com/mac-lucky/pushward-integrations/relay/internal/universal"

func init() {
	const (
		ogAlert   = "3f1c2a9e-7b4d-4e21-9c55-0a8d6e2b7f13"
		onCallURL = "https://oncall.example.net/a/grafana-oncall-app/alert-groups/IX7KQ2PLM5N8A"
		nrIssue   = "5c0d8e21-9a47-4f3b-8c16-2e7a9b40d5f8"
		nrURL     = "https://radar-api.service.newrelic.com/accounts/7654321/issues/" + nrIssue + "?notifier=WEBHOOK"
		azAlert   = "/subscriptions/0c5e1a7b-2d4f-4e8a-9b3c-6f1d2e8a7c40/providers/Microsoft.AlertsManagement/alerts/7d2b9e41-6c3a-4f5e-8b17-a9c0d4e2f615"
		gcpURL    = "https://console.cloud.google.com/monitoring/alerting/incidents/0.nq2kz81xtw4c?project=shop-prod"
	)
	addFixtures(map[string]want{
		"opsgenie_create.json": {
			preset: "opsgenie", title: "db-3 disk almost full", body: "Data volume on db-3 is at 91% and growing",
			corr: ogAlert, sev: universal.SeverityCritical, life: universal.LifecycleOngoing,
		},
		// Close carries no description, so the body falls back to the action.
		"opsgenie_close.json": {
			preset: "opsgenie", title: "db-3 disk almost full",
			corr: ogAlert, sev: universal.SeverityCritical, life: universal.LifecycleEnded,
		},
		"grafana-oncall_created.json": {
			preset: "grafana-oncall", title: "Checkout p95 latency above 2s", body: "Shop alerts",
			url: onCallURL, corr: "IX7KQ2PLM5N8A", life: universal.LifecycleOngoing,
		},
		"grafana-oncall_resolved.json": {
			preset: "grafana-oncall", title: "Checkout p95 latency above 2s", body: "Shop alerts",
			url: onCallURL, corr: "IX7KQ2PLM5N8A", life: universal.LifecycleEnded,
		},
		"betterstack-incident_started.json": {
			preset: "betterstack-incident", source: "betterstack", title: "Shop homepage", body: "Status 503",
		},
		"statuspage-incident_investigating.json": {
			preset: "statuspage-incident", title: "Elevated API error rates",
			body: "We are looking into elevated error rates on the public API.",
			url:  "https://stspg.io/x7k2m9p", corr: "k9pq4r2ts8vw",
			sev: universal.SeverityCritical, life: universal.LifecycleOngoing,
		},
		"statuspage-incident_resolved.json": {
			preset: "statuspage-incident", title: "Elevated API error rates", body: "Error rates are back to normal.",
			url: "https://stspg.io/x7k2m9p", corr: "k9pq4r2ts8vw",
			sev: universal.SeverityCritical, life: universal.LifecycleEnded,
		},
		"statuspage-component_major_outage.json": {
			preset: "statuspage-component", title: "Payments API", corr: "h3n8c5v2x7q1",
			sev: universal.SeverityCritical, life: universal.LifecycleOngoing,
		},
		"statuspage-component_operational.json": {
			preset: "statuspage-component", title: "Payments API", corr: "h3n8c5v2x7q1",
			sev: universal.SeverityInfo, life: universal.LifecycleEnded,
		},
		"updown_down.json": {
			preset: "updown", title: "DOWN: https://shop.example.org/ since 09:14:05 (UTC), reason: 502 Bad Gateway",
			url:  "https://updown.io/downtimes/6512ab34cd56ef7890ab12cd",
			corr: "6512ab34cd56ef7890ab12cd", life: universal.LifecycleOngoing,
		},
		"updown_up.json": {
			preset: "updown", title: "UP: https://shop.example.org/ since 09:26:40 (UTC), after being down for 12 minutes, reason: 502 Bad Gateway",
			url:  "https://updown.io/downtimes/6512ab34cd56ef7890ab12cd",
			corr: "6512ab34cd56ef7890ab12cd", life: universal.LifecycleEnded,
		},
		"pingdom_down.json": {
			preset: "pingdom", title: "Shop homepage", body: "Timeout (> 30s)", url: "https://shop.example.org/",
			corr: "7310552", sev: universal.SeverityCritical, life: universal.LifecycleOngoing,
		},
		"pingdom_up.json": {
			preset: "pingdom", title: "Shop homepage", body: "OK", url: "https://shop.example.org/",
			corr: "7310552", sev: universal.SeverityCritical, life: universal.LifecycleEnded,
		},
		"netdata-alert_critical.json": {
			preset: "netdata-alert", title: "disk_space_usage", body: "Disk space usage on db-3 is critical",
			url: "https://app.netdata.cloud/spaces/home-lab/rooms/all-nodes/alerts/disk_space_usage",
			sev: universal.SeverityCritical,
		},
		"netdata-reachability_unreachable.json": {
			preset: "netdata-reachability", source: "netdata", title: "db-3", body: "db-3 is unreachable",
			url: "https://app.netdata.cloud/spaces/home-lab/rooms/all-nodes/nodes/db-3",
			sev: universal.SeverityCritical,
		},
		"newrelic-classic_open.json": {
			preset: "newrelic-classic", source: "newrelic", title: "High CPU",
			body: "web-7 query result is > 90.0 for 5 minutes on 'High CPU'", url: nrURL,
			corr: nrIssue, sev: universal.SeverityCritical, life: universal.LifecycleOngoing,
		},
		"newrelic-classic_closed.json": {
			preset: "newrelic-classic", source: "newrelic", title: "High CPU",
			body: "web-7 query result is > 90.0 for 5 minutes on 'High CPU'", url: nrURL,
			corr: nrIssue, sev: universal.SeverityCritical, life: universal.LifecycleEnded,
		},
		// SNS carries the CloudWatch alarm as a JSON string, which Flatten
		// leaves as text: the body is that string.
		"aws-sns_alarm.json": {
			preset: "aws-sns", title: `ALARM: "api-5xx-rate" in EU (Ireland)`,
			body: `{"AlarmName":"api-5xx-rate","NewStateValue":"ALARM","NewStateReason":"Threshold Crossed: 1 datapoint [42.0] was greater than the threshold (10.0)."}`,
		},
		"azure-monitor_fired.json": {
			preset: "azure-monitor", title: "web-vm-cpu-high", body: "CPU above 85% on the web VMs",
			corr: azAlert, sev: universal.SeverityCritical, life: universal.LifecycleOngoing,
		},
		"azure-monitor_resolved.json": {
			preset: "azure-monitor", title: "web-vm-cpu-high", body: "CPU above 85% on the web VMs",
			corr: azAlert, sev: universal.SeverityCritical, life: universal.LifecycleEnded,
		},
		"gcp-monitoring_open.json": {
			preset: "gcp-monitoring", title: "Checkout latency",
			body: "Request latency for checkout-api is above the threshold of 2 with a value of 3.4.",
			url:  gcpURL, corr: "0.nq2kz81xtw4c", sev: universal.SeverityCritical, life: universal.LifecycleOngoing,
		},
		"gcp-monitoring_closed.json": {
			preset: "gcp-monitoring", title: "Checkout latency",
			body: "Request latency for checkout-api returned to normal with a value of 1.1.",
			url:  gcpURL, corr: "0.nq2kz81xtw4c", sev: universal.SeverityCritical, life: universal.LifecycleEnded,
		},
		"graylog_event.json": {
			preset: "graylog", title: "Failed SSH logins", body: "Failed SSH logins: count()=25.0",
			sev: universal.SeverityCritical,
		},
	})
}
