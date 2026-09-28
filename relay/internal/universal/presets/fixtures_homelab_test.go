package presets

import "github.com/mac-lucky/pushward-integrations/relay/internal/universal"

func init() {
	const (
		lidarrURL   = "http://lidarr.lan:8686"
		whisparrURL = "http://whisparr.lan:6969"
		harbour     = "The Quiet Harbour"
		lanterns    = "Lanterns on the Pier"
		cartography = "The Cartographer's Winter"
		scene       = "Evening Light"
		sceneRel    = "ExampleStudio.26.09.20.Evening.Light.1080p.WEB-DL"
		qbitMsg     = "Unable to communicate with qBittorrent. Connection refused"
		qbitWiki    = "https://wiki.servarr.com/lidarr/system#download-clients-are-unavailable-due-to-failures"
		rootMsg     = "Missing root folder: /media/whisparr"
		rootWiki    = "https://wiki.servarr.com/whisparr/system#missing-root-folder"
	)
	addFixtures(map[string]want{
		"lidarr-grab_grab.json": {
			preset: "lidarr-grab", source: "lidarr", title: lanterns,
			body: "The Quiet Harbour - Lanterns on the Pier (2024) [FLAC]", url: lidarrURL,
		},
		// The Test button sends the grab shape without a release.
		"lidarr-grab_test.json": {preset: "lidarr-grab", source: "lidarr", title: "Test title"},
		"lidarr-download_import.json": {
			preset: "lidarr-download", title: lanterns, body: harbour, url: lidarrURL,
		},
		"lidarr-import-failure_failure.json": {
			preset: "lidarr-import-failure", source: "lidarr", title: harbour, body: "ImportFailure", url: lidarrURL,
		},
		"lidarr-download-failure_failure.json": {
			preset: "lidarr-download-failure", title: "Northbound Static - Signal Fires (2023) [MP3 320]",
			body: "DownloadFailure",
		},
		"lidarr-health_issue.json": {
			preset: "lidarr-health", source: "lidarr", title: qbitMsg, body: "DownloadClientCheck", url: qbitWiki,
			corr: "DownloadClientCheck", sev: universal.SeverityCritical, life: universal.LifecycleOngoing,
		},
		"lidarr-health_restored.json": {
			preset: "lidarr-health", source: "lidarr", title: qbitMsg, body: "DownloadClientCheck", url: qbitWiki,
			corr: "DownloadClientCheck", sev: universal.SeverityCritical, life: universal.LifecycleEnded,
		},

		"readarr-grab_grab.json": {
			preset: "readarr-grab", source: "readarr", title: cartography,
			body: "Mara Ellison - The Cartographer's Winter (retail) (epub)",
		},
		"readarr-download_import.json": {preset: "readarr-download", title: cartography, body: "Mara Ellison"},
		"readarr-health_issue.json": {
			preset: "readarr-health", source: "readarr",
			title: "Indexers unavailable due to failures for more than 6 hours: Book Indexer",
			body:  "IndexerLongTermStatusCheck", url: "https://wiki.servarr.com/readarr/system#indexers-are-unavailable-due-to-failures",
			sev: universal.SeverityWarning,
		},

		"whisparr-grab_grab.json": {
			preset: "whisparr-grab", source: "whisparr", title: scene, body: sceneRel, url: whisparrURL,
		},
		"whisparr-download_import.json": {
			preset: "whisparr-download", source: "whisparr", title: scene, body: "Example Studio", url: whisparrURL,
		},
		"whisparr-v3-grab_grab.json": {
			preset: "whisparr-v3-grab", title: scene, body: sceneRel, url: whisparrURL,
		},
		"whisparr-v3-download_import.json": {
			preset: "whisparr-v3-download", source: "whisparr", title: scene, body: "WEBDL-1080p",
		},
		"whisparr-health_issue.json": {
			preset: "whisparr-health", source: "whisparr", title: rootMsg, body: "RootFolderCheck", url: rootWiki,
			corr: "RootFolderCheck", sev: universal.SeverityWarning, life: universal.LifecycleOngoing,
		},
		"whisparr-health_restored.json": {
			preset: "whisparr-health", source: "whisparr", title: rootMsg, body: "RootFolderCheck", url: rootWiki,
			corr: "RootFolderCheck", sev: universal.SeverityWarning, life: universal.LifecycleEnded,
		},

		"nextcloud-node_created.json": {
			preset: "nextcloud-node", title: "/jana/files/Scans/receipt-hardware-store.pdf",
			body: `OCP\Files\Events\Node\NodeCreatedEvent`,
		},
		// A deleted node has no id, and a system job has no user.
		"nextcloud-node_deleted.json": {
			preset: "nextcloud-node", source: "nextcloud", title: "/jana/files/Scans/old-invoice.pdf",
			body: `OCP\Files\Events\Node\NodeDeletedEvent`,
		},
		"nextcloud-node-pair_renamed.json": {
			preset: "nextcloud-node-pair", title: "/jana/files/Receipts/2026-09 hardware store.pdf",
			body: `OCP\Files\Events\Node\NodeRenamedEvent`,
		},
		"nextcloud-form_submitted.json": {preset: "nextcloud-form", title: "Garden plot sign-up", body: "Piotr"},
		"nextcloud-mail_received.json": {
			preset: "nextcloud-mail", title: "Boiler service booked for Thursday",
			body: `OCA\Mail\Event\NewMessageReceivedEvent`,
		},

		"watchtower_update.json": {
			preset: "watchtower", source: "watchtower", title: "Watchtower updates on media-host",
			body: "Found new image for linuxserver/lidarr:latest (1a2b3c4d5e6f)\nStopping /lidarr (9f8e7d6c5b4a) with SIGTERM\nCreating /lidarr",
		},
	})
}
