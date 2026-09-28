package relaytest

import (
	"encoding/json"
	"net/http"
)

// SenderHeaders returns the headers the service behind fixture directory dir
// sends with payload, beyond the content type and the key. They are what the
// root route sees of a real sender: the *arr apps name themselves in the
// User-Agent, and Gitea and Forgejo send their event headers (Forgejo also
// sends Gitea's, Gogs' and GitHub's, as Gitea does).
func SenderHeaders(dir string, payload []byte) http.Header {
	h := http.Header{}
	ua := func(v string) { h.Set("User-Agent", v) }
	switch dir {
	case "radarr":
		ua("Radarr/5.14.0.9383 (ubuntu 22.04)")
	case "sonarr":
		ua("Sonarr/4.0.10.2544 (ubuntu 22.04)")
	case "prowlarr":
		ua("Prowlarr/1.25.4.4818 (ubuntu 22.04)")
	case "gitea":
		ua("Go-http-client/1.1")
		forgeEvent(h, giteaEvent(payload), "X-Gitea-Event", "X-Gitea-Event-Type", "X-Gogs-Event", "X-GitHub-Event", "X-GitHub-Event-Type")
		h.Set("X-Gitea-Delivery", "00000000-0000-4000-8000-000000000401")
	case "forgejo":
		ua("Go-http-client/1.1")
		var p struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(payload, &p)
		forgeEvent(h, "action_run_"+p.Action, "X-Forgejo-Event", "X-Forgejo-Event-Type", "X-Gitea-Event", "X-Gitea-Event-Type",
			"X-Gogs-Event", "X-GitHub-Event", "X-GitHub-Event-Type")
		h.Set("X-Forgejo-Delivery", "00000000-0000-4000-8000-000000000402")
	case "grafana":
		ua("Grafana")
	case "jellyfin":
		ua("Jellyfin-Server/10.9.0")
	case "overseerr", "uptimekuma":
		ua("axios/1.7.7")
	case "paperless":
		ua("python-httpx/0.27.2")
	case "changedetection", "bazarr", "unmanic":
		// Apprise's json:// plugin.
		ua("Apprise")
	case "truenas":
		ua("Python/3.11 aiohttp/3.9.5")
	case "komodo", "proxmox":
		// Rust HTTP clients that set no User-Agent.
	default:
		// argocd, backrest, gatus and the universal fixtures: Go senders.
		ua("Go-http-client/1.1")
	}
	return h
}

// giteaEvent is the event name Gitea sends for payload.
func giteaEvent(payload []byte) string {
	var p struct {
		WorkflowRun json.RawMessage `json:"workflow_run"`
		WorkflowJob json.RawMessage `json:"workflow_job"`
	}
	_ = json.Unmarshal(payload, &p)
	switch {
	case p.WorkflowJob != nil:
		return "workflow_job"
	case p.WorkflowRun != nil:
		return "workflow_run"
	}
	return "push"
}

func forgeEvent(h http.Header, event string, names ...string) {
	for _, n := range names {
		h.Set(n, event)
	}
}
