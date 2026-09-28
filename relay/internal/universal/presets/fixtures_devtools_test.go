package presets

import "github.com/mac-lucky/pushward-integrations/relay/internal/universal"

// Code hosting, CI and error tracking. A run that is still going has no
// conclusion yet, so its body falls back to the lifecycle value.
func init() {
	const (
		ongoing  = universal.LifecycleOngoing
		ended    = universal.LifecycleEnded
		critical = universal.SeverityCritical
		warning  = universal.SeverityWarning
		info     = universal.SeverityInfo
	)
	addFixtures(map[string]want{
		// GitHub
		"github-push_push.json": {
			preset: "github-push", title: "lunar-kite/tidepool", body: "Retry uploads on 503 from the blob store",
			url: "https://github.com/lunar-kite/tidepool/compare/4f2c9a1e7b3d...9b8a7c6d5e4f",
		},
		"github-pull-request_opened.json": {
			preset: "github-pull-request", title: "Add a dry-run flag to the sync command", body: "opened",
			url: "https://github.com/lunar-kite/tidepool/pull/214",
		},
		"github-issues_opened.json": {
			preset: "github-issues", title: "Sync hangs when the target bucket is empty", body: "opened",
			url: "https://github.com/lunar-kite/tidepool/issues/219",
		},
		"github-release_published.json": {
			preset: "github-release", title: "v3.2.0", body: "published",
			url: "https://github.com/lunar-kite/tidepool/releases/tag/v3.2.0", corr: "174209331",
		},
		"github-check-run_created.json": {
			preset: "github-check-run", title: "lint", body: "sync-dry-run",
			url: "https://github.com/lunar-kite/tidepool/runs/28817340129", corr: "28817340129", life: ongoing, lifeTable: true,
		},
		"github-check-run_completed.json": {
			preset: "github-check-run", title: "lint", body: "sync-dry-run",
			url: "https://github.com/lunar-kite/tidepool/runs/28817340129", corr: "28817340129", life: ended,
		},
		"github-workflow-run_in_progress.json": {
			preset: "github-workflow-run", title: "lunar-kite/tidepool", body: "CI",
			url: "https://github.com/lunar-kite/tidepool/actions/runs/11839204561", corr: "11839204561", life: ongoing, lifeTable: true,
		},
		"github-workflow-run_completed.json": {
			preset: "github-workflow-run", title: "lunar-kite/tidepool", body: "CI",
			url: "https://github.com/lunar-kite/tidepool/actions/runs/11839204561", corr: "11839204561", life: ended,
		},

		// GitLab
		"gitlab-push_push.json": {
			preset: "gitlab-push", title: "harbourline/ledger-api", body: "refs/heads/main",
			url: "https://gitlab.example.com/harbourline/ledger-api",
		},
		"gitlab-merge-request_open.json": {
			preset: "gitlab-merge-request", title: "Round payout totals to the cent", body: "open",
			url: "https://gitlab.example.com/harbourline/ledger-api/-/merge_requests/58",
		},
		"gitlab-issue_open.json": {
			preset: "gitlab-issue", title: "Payout export drops the last row", body: "open",
			url: "https://gitlab.example.com/harbourline/ledger-api/-/issues/91",
		},
		"gitlab-pipeline_running.json": {
			preset: "gitlab-pipeline", title: "harbourline/ledger-api", body: "main",
			url:  "https://gitlab.example.com/harbourline/ledger-api/-/pipelines/1482093316",
			corr: "1482093316", life: ongoing,
		},
		"gitlab-pipeline_failed.json": {
			preset: "gitlab-pipeline", title: "harbourline/ledger-api", body: "main",
			url:  "https://gitlab.example.com/harbourline/ledger-api/-/pipelines/1482093316",
			corr: "1482093316", life: ended,
		},
		"gitlab-deployment_running.json": {
			preset: "gitlab-deployment", title: "harbourline/ledger-api", body: "production",
			url:  "https://gitlab.example.com/harbourline/ledger-api/-/jobs/7710301250",
			corr: "612004583", life: ongoing,
		},
		"gitlab-deployment_success.json": {
			preset: "gitlab-deployment", title: "harbourline/ledger-api", body: "production",
			url:  "https://gitlab.example.com/harbourline/ledger-api/-/jobs/7710301250",
			corr: "612004583", life: ended,
		},

		// Bitbucket Cloud
		"bitbucket-push_push.json": {
			preset: "bitbucket-push", title: "northwind-labs/route-planner", body: "Skip closed depots when planning routes",
			url: "https://bitbucket.org/northwind-labs/route-planner/branches/compare/3e8f1a2b4c6d..b1c2d3e4f5a6",
		},
		"bitbucket-pullrequest_created.json": {
			preset: "bitbucket-pullrequest", title: "Cache geocoder lookups for an hour", body: "OPEN",
			url: "https://bitbucket.org/northwind-labs/route-planner/pull-requests/37",
		},
		"bitbucket-commit-status_failed.json": {
			preset: "bitbucket-commit-status", title: "Integration tests", body: "FAILED",
			url: "https://ci.example.com/route-planner/builds/4410", sev: warning,
		},

		// Sentry
		"sentry-issue_created.json": {
			preset: "sentry-issue", title: "TypeError: Cannot read properties of undefined (reading 'blocks')",
			body: "renderOutline(src/outline/render)", url: "https://quillmark.sentry.io/issues/5281937406/",
			corr: "5281937406", sev: warning, life: ongoing,
		},
		"sentry-issue_resolved.json": {
			preset: "sentry-issue", title: "TypeError: Cannot read properties of undefined (reading 'blocks')",
			body: "renderOutline(src/outline/render)", url: "https://quillmark.sentry.io/issues/5281937406/",
			corr: "5281937406", sev: warning, life: ended,
		},
		"sentry-metric-alert_critical.json": {
			preset: "sentry-metric-alert", title: "Checkout errors",
			body: "212 events in the last 10 minutes\nFilter: transaction:/checkout",
			url:  "https://quillmark.sentry.io/alerts/412/", corr: "88104", sev: critical, life: ongoing,
		},
		"sentry-metric-alert_resolved.json": {
			preset: "sentry-metric-alert", title: "Checkout errors",
			body: "212 events in the last 10 minutes\nFilter: transaction:/checkout",
			url:  "https://quillmark.sentry.io/alerts/412/", corr: "88104", sev: info, life: ended,
		},
		"sentry-event-alert_triggered.json": {
			preset: "sentry-event-alert", title: "KeyError: 'currency'", body: "sync_ledger(billing/tasks)",
			url:  "https://quillmark.sentry.io/issues/5281990123/events/c3f0a9e2b71d4e58a6c4d2f1e0b9a8c7/",
			corr: "5281990123", sev: warning,
		},
		"sentry-legacy-webhook_alert.json": {
			preset: "sentry-legacy-webhook", title: "ValueError: amount must be positive", body: "reconcile(billing/reconcile)",
			url:  "https://quillmark.sentry.io/issues/5282004417/?referrer=webhooks_plugin",
			corr: "5282004417", sev: warning,
		},

		// Honeybadger
		"honeybadger-fault_occurred.json": {
			preset: "honeybadger-fault", title: "Redis::TimeoutError", body: "Connection timed out",
			url: "https://app.honeybadger.io/projects/118204/faults/4815162", corr: "4815162", life: ongoing,
		},
		"honeybadger-fault_resolved.json": {
			preset: "honeybadger-fault", title: "Redis::TimeoutError", body: "Connection timed out",
			url: "https://app.honeybadger.io/projects/118204/faults/4815162", corr: "4815162", life: ended,
		},
		"honeybadger-uptime_down.json": {
			preset: "honeybadger-uptime", title: "Tracking page", body: "[Parcelpost] Tracking page is down.",
			url:  "https://app.honeybadger.io/projects/118204/sites/6b1f0c3e-8a2d-4e7f-9c51-2d8e4a6b0f13",
			corr: "6b1f0c3e-8a2d-4e7f-9c51-2d8e4a6b0f13", life: ongoing,
		},
		"honeybadger-uptime_up.json": {
			preset: "honeybadger-uptime", title: "Tracking page", body: "[Parcelpost] Tracking page is back up.",
			url:  "https://app.honeybadger.io/projects/118204/sites/6b1f0c3e-8a2d-4e7f-9c51-2d8e4a6b0f13",
			corr: "6b1f0c3e-8a2d-4e7f-9c51-2d8e4a6b0f13", life: ended,
		},
		"honeybadger-check-in_missing.json": {
			preset: "honeybadger-check-in", title: "Nightly label purge",
			body: "[Parcelpost] MISSING: Nightly label purge hasn't checked in for 26 hours", corr: "k3Ptw9", life: ongoing,
		},
		"honeybadger-check-in_reporting.json": {
			preset: "honeybadger-check-in", title: "Nightly label purge",
			body: "[Parcelpost] REPORTING: Nightly label purge is reporting again", corr: "k3Ptw9", life: ended,
		},

		// Rollbar
		"rollbar-item_new_item.json": {
			preset: "rollbar-item", title: "ReferenceError: basketTotal is not defined", body: "production",
			url:  "https://rollbar.com/fernhill/checkout-web/items/318/",
			corr: "1392047710", sev: warning, life: ongoing,
		},
		"rollbar-item_resolved_item.json": {
			preset: "rollbar-item", title: "ReferenceError: basketTotal is not defined", body: "production",
			url:  "https://rollbar.com/fernhill/checkout-web/items/318/",
			corr: "1392047710", sev: warning, life: ended,
		},

		// CircleCI, Semaphore, Buildkite, Drone, Jenkins
		"circleci-workflow_completed.json": {
			preset: "circleci-workflow", title: "fare-engine", body: "failed", sev: warning,
			url: "https://app.circleci.com/pipelines/gh/copperline/fare-engine/1207/workflows/c1e3a5b7-9d0f-4b2c-8e4a-6f8b0d2e4a6c",
		},
		"circleci-job_completed.json": {
			preset: "circleci-job", title: "unit-tests", body: "success", sev: info,
			url: "https://app.circleci.com/pipelines/gh/copperline/fare-engine/1207/workflows/c1e3a5b7-9d0f-4b2c-8e4a-6f8b0d2e4a6c",
		},
		"semaphore-pipeline_done.json": {
			preset: "semaphore-pipeline", title: "tide-tables", body: "failed", sev: warning,
		},
		"buildkite-build_running.json": {
			preset: "buildkite-build", title: "Mobile app", body: "main",
			url:  "https://buildkite.com/pinecrest/mobile-app/builds/2291",
			corr: "01923f4a-7c2e-4d6b-9a1f-3e5c7b9d1f24", life: ongoing,
		},
		"buildkite-build_finished.json": {
			preset: "buildkite-build", title: "Mobile app", body: "main",
			url:  "https://buildkite.com/pinecrest/mobile-app/builds/2291",
			corr: "01923f4a-7c2e-4d6b-9a1f-3e5c7b9d1f24", life: ended,
		},
		"drone-build_created.json": {
			preset: "drone-build", title: "birchwood/inventory", body: "main", corr: "20471", life: ongoing,
		},
		"drone-build_updated.json": {
			preset: "drone-build", title: "birchwood/inventory", body: "main", corr: "20471", life: ended,
		},
		// No status until the build completes, so no body either.
		"jenkins-notification_started.json": {
			preset: "jenkins-notification", title: "payments-nightly",
			url:  "https://jenkins.example.com/job/payments-nightly/482/",
			corr: "job/payments-nightly/482/", life: ongoing,
		},
		"jenkins-notification_completed.json": {
			preset: "jenkins-notification", title: "payments-nightly", body: "FAILURE",
			url:  "https://jenkins.example.com/job/payments-nightly/482/",
			corr: "job/payments-nightly/482/", life: ended,
		},

		// Deploys, registries, GitOps
		"netlify-deploy_building.json": {
			preset: "netlify-deploy", title: "quiet-otter-docs", body: "building",
			url:  "https://app.netlify.com/projects/quiet-otter-docs",
			corr: "66f7c1a2e4b09d0008a3c5e7", life: ongoing,
		},
		"netlify-deploy_error.json": {
			preset: "netlify-deploy", title: "quiet-otter-docs",
			body: "Failed during stage 'building site': Build script returned non-zero exit code: 2",
			url:  "https://app.netlify.com/projects/quiet-otter-docs",
			corr: "66f7c1a2e4b09d0008a3c5e7", life: ended,
		},
		"dockerhub-push_push.json": {
			preset: "dockerhub-push", title: "fieldnote/scanner", body: "2.8.1",
			url: "https://hub.docker.com/r/fieldnote/scanner",
		},
		"harbor-artifact_push.json": {
			preset: "harbor-artifact", title: "platform/ingest-gateway", body: "PUSH_ARTIFACT",
		},
		"harbor-cloudevents_scan.json": {
			preset: "harbor-cloudevents", title: "platform/ingest-gateway", body: "harbor.scan.completed",
		},
		"flux-event_error.json": {
			preset: "flux-event", title: "billing", sev: warning,
			body: "health check failed after 5m0s: timeout waiting for: [Deployment/billing/billing-api status: 'InProgress']",
		},
	})
}
