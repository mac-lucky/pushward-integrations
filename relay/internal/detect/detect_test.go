package detect

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name   string
		h      http.Header
		body   string
		route  string // "" for no match
		rule   string
		via    string
		vetoed bool
	}{
		{"radarr ua", hdr("User-Agent", "Radarr/5.14.0.9383 (ubuntu 22.04)"), `{"eventType":"Grab"}`, "/radarr", "radarr", ViaHeader, false},
		{"sonarr ua", hdr("User-Agent", "Sonarr/4.0.10"), `{"eventType":"Test"}`, "/sonarr", "sonarr", ViaHeader, false},
		{"prowlarr ua", hdr("User-Agent", "Prowlarr/1.25"), `{"eventType":"Health"}`, "/prowlarr", "prowlarr", ViaHeader, false},
		{"radarr ua without eventType", hdr("User-Agent", "Radarr/5.14"), `{"title":"x","body":"y"}`, "", "starr-other", ViaVeto, true},
		{"radarr ua, numeric eventType", hdr("User-Agent", "Radarr/5.14"), `{"eventType":3}`, "", "starr-other", ViaVeto, true},
		{"lidarr ua", hdr("User-Agent", "Lidarr/2.5.3"), `{"eventType":"Grab"}`, "", "starr-other", ViaVeto, true},
		{"readarr ua", hdr("User-Agent", "Readarr/0.4"), `{"eventType":"Grab"}`, "", "starr-other", ViaVeto, true},
		{"whisparr ua", hdr("User-Agent", "Whisparr/2.0"), `{"eventType":"Grab"}`, "", "starr-other", ViaVeto, true},
		{"ua is matched as a prefix only", hdr("User-Agent", "NotRadarr/1.0"), `{"eventType":"Grab"}`, "", "", "", false},

		{"gitea workflow_run", hdr("X-Gitea-Event", "workflow_run"), `{"action":"requested","workflow_run":{"id":1}}`, "/gitea", "gitea-actions", ViaHeader, false},
		{"gitea workflow_job", hdr("X-Gitea-Event", "workflow_job"), `{"action":"queued","workflow_job":{"id":1}}`, "/gitea", "gitea-actions", ViaHeader, false},
		{"forgejo workflow_job", hdr("X-Forgejo-Event", "workflow_job"), `{"workflow_job":{"id":1}}`, "/gitea", "gitea-actions", ViaHeader, false},
		{"forgejo run", hdr("X-Forgejo-Event", "action_run_failure", "X-Gitea-Event", "action_run_failure"), `{"action":"failure","run":{"id":56}}`, "/forgejo", "forgejo-actions", ViaHeader, false},
		{"gitea run is not forgejo", hdr("X-Gitea-Event", "action_run_failure"), `{"action":"failure","run":{"id":56}}`, "", "gitea-other", ViaVeto, true},
		{"gitea push", hdr("X-Gitea-Event", "push", "X-GitHub-Event", "push"), `{"ref":"refs/heads/main"}`, "", "gitea-other", ViaVeto, true},
		{"gitea workflow_run not an object", hdr("X-Gitea-Event", "workflow_run"), `{"workflow_run":null}`, "", "gitea-other", ViaVeto, true},
		{"github workflow_run", hdr("X-GitHub-Event", "workflow_run"), `{"workflow_run":{"id":1}}`, "", "github", ViaVeto, true},
		{"gogs", hdr("X-Gogs-Event", "push"), `{"ref":"refs/heads/main"}`, "", "gogs", ViaVeto, true},
		{"gitlab", hdr("X-Gitlab-Event", "Pipeline Hook"), `{"object_kind":"pipeline"}`, "", "gitlab", ViaVeto, true},
		{"bitbucket", hdr("X-Event-Key", "repo:push"), `{"push":{}}`, "", "bitbucket", ViaVeto, true},
		{"sentry", hdr("Sentry-Hook-Resource", "issue"), `{"action":"created"}`, "", "sentry", ViaVeto, true},

		{"grafana v1", nil, `{"alerts":[],"groupKey":"{}","version":"1"}`, "/grafana", "grafana", ViaBody, false},
		{"grafana by orgId", nil, `{"alerts":[],"groupKey":"{}","orgId":1}`, "/grafana", "grafana", ViaBody, false},
		{"alertmanager v4", nil, `{"alerts":[],"groupKey":"{}","version":"4"}`, "", "", "", false},
		{"alertmanager v4 with orgId", nil, `{"alerts":[],"groupKey":"{}","version":"4","orgId":1}`, "", "", "", false},
		{"grafana without groupKey", nil, `{"alerts":[],"version":"1"}`, "", "", "", false},
		{"argocd", nil, `{"app":"web","event":"sync-failed"}`, "/argocd", "argocd", ViaBody, false},
		{"argocd empty app", nil, `{"app":"","event":"sync-failed"}`, "", "", "", false},
		{"argocd unknown event", nil, `{"app":"web","event":"created"}`, "", "", "", false},
		{"backrest", nil, `{"event":"CONDITION_SNAPSHOT_START"}`, "/backrest", "backrest", ViaBody, false},
		{"backrest lowercase", nil, `{"event":"condition_snapshot_start"}`, "", "", "", false},
		{"changedetection preview_url", nil, `{"diff_url":"https://x/d","preview_url":"https://x/p"}`, "/changedetection", "changedetection", ViaBody, false},
		{"changedetection diff_url alone", nil, `{"diff_url":"https://x/d"}`, "", "", "", false},
		{"gatus", nil, `{"endpoint_name":"api","status":"RESOLVED"}`, "/gatus", "gatus", ViaBody, false},
		{"gatus other status", nil, `{"endpoint_name":"api","status":"UP"}`, "", "", "", false},
		{"jellyfin", nil, `{"NotificationType":"ItemAdded","ServerName":"home"}`, "/jellyfin", "jellyfin", ViaBody, false},
		{"jellyfin without server", nil, `{"NotificationType":"ItemAdded"}`, "", "", "", false},
		{"komodo", nil, `{"ts":1,"resolved":false,"level":"OK","target":{},"data":{"type":"Test"}}`, "/komodo", "komodo", ViaBody, false},
		{"komodo data without type", nil, `{"ts":1,"resolved":false,"level":"OK","target":{},"data":{}}`, "", "", "", false},
		{"komodo string ts", nil, `{"ts":"1","resolved":false,"level":"OK","target":{},"data":{"type":"Test"}}`, "", "", "", false},
		{"overseerr", nil, `{"notification_type":"MEDIA_AUTO_APPROVED","subject":"Dune"}`, "/overseerr", "overseerr", ViaBody, false},
		{"overseerr without subject", nil, `{"notification_type":"TEST_NOTIFICATION"}`, "", "", "", false},
		{"overseerr other type", nil, `{"notification_type":"USER_CREATED","subject":"x"}`, "", "", "", false},
		{"paperless added", nil, `{"event":"added","doc_id":42}`, "/paperless", "paperless", ViaBody, false},
		{"paperless string doc_id", nil, `{"event":"added","doc_id":"42"}`, "", "", "", false},
		{"paperless consumption", nil, `{"event":"consumption_started","filename":"scan.pdf"}`, "/paperless", "paperless", ViaBody, false},
		{"proxmox", nil, `{"type":"vzdump","title":"t","message":"m","severity":"info","hostname":"pve"}`, "/proxmox", "proxmox", ViaBody, false},
		{"proxmox empty type", nil, `{"type":"","title":"t","message":"m","severity":"notice","hostname":"pve"}`, "/proxmox", "proxmox", ViaBody, false},
		{"proxmox other severity", nil, `{"type":"vzdump","title":"t","message":"m","severity":"critical","hostname":"pve"}`, "", "", "", false},
		{"proxmox type system", nil, `{"type":"system","title":"t","message":"m","severity":"error","hostname":"pve"}`, "", "", "", false},
		{"proxmox type test", nil, `{"type":"test","title":"t","message":"m","severity":"info","hostname":"pve"}`, "", "", "", false},
		{"proxmox no hostname", nil, `{"type":"vzdump","title":"t","message":"m","severity":"info"}`, "", "", "", false},
		{"uptimekuma", nil, `{"msg":"down","heartbeat":{"status":0},"monitor":{"id":1}}`, "/uptimekuma", "uptimekuma", ViaBody, false},
		{"uptimekuma test", nil, `{"msg":"test","heartbeat":null,"monitor":null}`, "/uptimekuma", "uptimekuma", ViaBody, false},
		{"uptimekuma one null", nil, `{"msg":"x","heartbeat":null,"monitor":{"id":1}}`, "", "", "", false},
		{"uptimekuma string status", nil, `{"msg":"x","heartbeat":{"status":"down"},"monitor":{}}`, "", "", "", false},
		{"bazarr", nil, `{"version":"1.0","title":"Bazarr notification","message":"m","type":"info"}`, "/bazarr", "bazarr", ViaBody, false},
		{"unmanic", nil, `{"version":"1.0","title":"Unmanic - Task Failed","message":"m","type":"failure"}`, "/unmanic", "unmanic", ViaBody, false},
		{"apprise from someone else", nil, `{"version":"1.0","title":"Backup done","message":"m","type":"info"}`, "", "", "", false},

		{"array body", nil, `[{"endpoint_name":"api","status":"RESOLVED"}]`, "", "", "", false},
		{"string body", nil, `"hello"`, "", "", "", false},
		{"invalid json", nil, `{"endpoint_name":"api",`, "", "", "", false},
		{"empty body", nil, ``, "", "", "", false},
		{"nested match does not count", nil, `{"payload":{"endpoint_name":"api","status":"RESOLVED"}}`, "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := Detect(tt.h, []byte(tt.body))
			if ok != (tt.route != "") {
				t.Fatalf("Detect = %+v, %v; want route %q", m, ok, tt.route)
			}
			if m.Route != tt.route || m.Rule != tt.rule || m.Via != tt.via {
				t.Errorf("Detect = %+v, want {Route:%s Rule:%s Via:%s}", m, tt.route, tt.rule, tt.via)
			}
			if vetoed := !ok && m.Via == ViaVeto; vetoed != tt.vetoed {
				t.Errorf("vetoed = %v, want %v", vetoed, tt.vetoed)
			}
		})
	}
}

// TestVetoesBeatBodyRules pairs each veto with a body some body rule claims,
// so a veto that stopped applying would show as a match.
func TestVetoesBeatBodyRules(t *testing.T) {
	tests := []struct {
		name string
		h    http.Header
		body string
		rule string
	}{
		{"radarr ua without eventType", hdr("User-Agent", "Radarr/5.14"), `{"endpoint_name":"api","status":"TRIGGERED"}`, "starr-other"},
		{"lidarr", hdr("User-Agent", "Lidarr/2.5.3"), `{"eventType":"Grab","app":"music","event":"deployed"}`, "starr-other"},
		{"readarr", hdr("User-Agent", "Readarr/0.4"), `{"eventType":"Grab","event":"CONDITION_SNAPSHOT_START"}`, "starr-other"},
		{"whisparr", hdr("User-Agent", "Whisparr/2.0"), `{"eventType":"Test","endpoint_name":"api","status":"RESOLVED"}`, "starr-other"},
		{"gitea push", hdr("X-Gitea-Event", "push"), `{"app":"web","event":"sync-failed"}`, "gitea-other"},
		{"forgejo issue", hdr("X-Forgejo-Event", "issues"), `{"NotificationType":"ItemAdded","ServerName":"home"}`, "gitea-other"},
		{"github", hdr("X-GitHub-Event", "ping"), `{"endpoint_name":"api","status":"TRIGGERED"}`, "github"},
		{"gogs", hdr("X-Gogs-Event", "push"), `{"alerts":[],"groupKey":"{}","version":"1"}`, "gogs"},
		{"gitlab", hdr("X-Gitlab-Event", "Pipeline Hook"), `{"event":"added","doc_id":1}`, "gitlab"},
		{"bitbucket", hdr("X-Event-Key", "repo:push"), `{"diff_url":"https://x/d","url":"https://x"}`, "bitbucket"},
		{"sentry", hdr("Sentry-Hook-Resource", "issue"), `{"version":"1.0","title":"Unmanic - x","message":"m","type":"info"}`, "sentry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := Detect(nil, []byte(tt.body)); !ok {
				t.Fatal("no body rule claims the body, so the row tests nothing")
			}
			m, ok := Detect(tt.h, []byte(tt.body))
			if ok || m.Via != ViaVeto || m.Rule != tt.rule {
				t.Errorf("Detect = %+v, %v; want a %s veto", m, ok, tt.rule)
			}
		})
	}
}

func TestRoutesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Routes() {
		if seen[r] {
			t.Errorf("route %s listed twice", r)
		}
		seen[r] = true
	}
}

func FuzzDetect(f *testing.F) {
	f.Add("", []byte(`{"alerts":[],"groupKey":"{}","version":"1"}`))
	f.Add("Radarr/5", []byte(`{"eventType":"Grab"}`))
	f.Add("", []byte(`{"msg":"x","heartbeat":{"status":0},"monitor":{}}`))
	f.Add("", []byte(`{"ts":1,"resolved":true,"level":"OK","target":{},"data":{"type":"x"}}`))
	f.Fuzz(func(t *testing.T, ua string, body []byte) {
		m, ok := Detect(hdr("User-Agent", ua), body)
		if ok && (m.Route == "" || m.Via == ViaVeto) {
			t.Fatalf("match without a route: %+v", m)
		}
		if !ok && m.Route != "" {
			t.Fatalf("no match but a route: %+v", m)
		}
	})
}

// FuzzMembers checks the index against encoding/json decoding the same body
// into a map of raw values.
func FuzzMembers(f *testing.F) {
	for _, s := range []string{
		`{}`, `null`, `[1]`, `"x"`, ` {"a" : 1 , "b":[1,{"c":"]"}],"d":"\"}"} `,
		`{"a":1,"a":2}`, `{"a\u0062":1,"ab":2}`, `{"\u00e9":{"x":null},"k":-1.5e3}`,
		`{"a":"b\\"}`, "{\"\xff\":true}", `{"a":1`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		got := members(body)
		var want map[string]json.RawMessage
		if err := json.Unmarshal(body, &want); err != nil {
			want = nil
		}
		if (got == nil) != (want == nil) || len(got) != len(want) {
			t.Fatalf("members(%q) = %q, want %q", body, got, want)
		}
		for k, w := range want {
			if g, ok := got[k]; !ok || !bytes.Equal(g, w) {
				t.Fatalf("members(%q)[%q] = %q, want %q", body, k, g, w)
			}
		}
	})
}
