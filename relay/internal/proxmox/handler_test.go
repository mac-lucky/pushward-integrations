package proxmox

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

func testConfig() *config.ProxmoxConfig {
	return &config.ProxmoxConfig{
		BaseProviderConfig: config.BaseProviderConfig{
			Enabled:        true,
			Priority:       2,
			CleanupDelay:   1 * time.Hour,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
	}
}

func newHandler(t *testing.T, cfg *config.ProxmoxConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL, nil)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, store, pool, cfg)

	return mux, calls, mu
}

func send(t *testing.T, h http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/proxmox", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// The vzdump_* and replication_failure fixtures follow PVE's own notification
// templates. backup_start, backup_success and backup_failure are an older
// synthetic shape PVE never sends; they stay only because the universal
// ranker's golden (written by pushward-classifier) reads them by name.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../../testdata/proxmox", name)) // #nosec G304 -- fixture path under relay/testdata
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assertOpenedAndEnded checks the four calls a job outcome makes on its own:
// create, the outcome frame at once, then the ender's two phases.
func assertOpenedAndEnded(t *testing.T, recorded []testutil.APICall, slug, name, state, accent string) {
	t.Helper()
	if len(recorded) != 4 {
		t.Fatalf("expected create + ONGOING + phase1 + phase2 = 4 calls, got %d", len(recorded))
	}
	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" || create.Slug != slug {
		t.Errorf("expected POST /activities for %s, got %s %s slug %s", slug, recorded[0].Method, recorded[0].Path, create.Slug)
	}
	if create.Name != name {
		t.Errorf("expected name %q, got %q", name, create.Name)
	}
	for i, want := range []string{pushward.StateOngoing, pushward.StateOngoing, pushward.StateEnded} {
		c := recorded[i+1]
		if c.Path != "/activities/"+slug {
			t.Errorf("call %d: expected PATCH /activities/%s, got %s %s", i+1, slug, c.Method, c.Path)
		}
		var u pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &u)
		if u.State != want || u.Content.State != state || u.Content.AccentColor != accent {
			t.Errorf("call %d: got %s %q %s, want %s %q %s", i+1, u.State, u.Content.State, u.Content.AccentColor, want, state, accent)
		}
		if u.Content.Template != "steps" || u.Content.CurrentStep == nil || *u.Content.CurrentStep != 2 {
			t.Errorf("call %d: expected steps template at step 2", i+1)
		}
	}
}

// PVE sends one vzdump notification when a job ends and none when it starts,
// so each outcome opens its card and ends it. The job log in the message says
// "Starting Backup of VM ..." every time; it must not reopen a running card.
func TestBackupOutcomeOpensAndEndsCard(t *testing.T) {
	for _, tc := range []struct {
		file, jobID, name, state, accent string
	}{
		{"vzdump_success.json", "backup-6d2f0a1c-4b3e", "vzdump backup status (pve1.home.arpa): backup successful", "Backup Complete", pushward.ColorGreen},
		{"vzdump_failure.json", "backup-0b7e93d4-a1f2", "vzdump backup status (pve1.home.arpa): backup failed", "Backup Failed", pushward.ColorRed},
		// One notification for three guests: one card, keyed on the job.
		{"vzdump_multi_guest.json", "backup-a91c3e57-02d8", "vzdump backup status (pve1.home.arpa): backup successful", "Backup Complete", pushward.ColorGreen},
	} {
		t.Run(tc.file, func(t *testing.T) {
			h, calls, mu := newHandler(t, testConfig())
			if w := send(t, h, fixture(t, tc.file)); w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			time.Sleep(100 * time.Millisecond)
			assertOpenedAndEnded(t, testutil.GetCalls(calls, mu),
				text.SlugHash("proxmox-backup", "pve1:"+tc.jobID, 4), tc.name, tc.state, tc.accent)
		})
	}
}

func TestReplicationFailureOpensAndEndsCard(t *testing.T) {
	body := fixture(t, "replication_failure.json")
	// A body template without job_id still finds the job in the message.
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "job_id")
	noJobID, _ := json.Marshal(m)

	for name, payload := range map[string]string{"job_id": body, "no job_id": string(noJobID)} {
		t.Run(name, func(t *testing.T) {
			h, calls, mu := newHandler(t, testConfig())
			if w := send(t, h, payload); w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			time.Sleep(100 * time.Millisecond)
			assertOpenedAndEnded(t, testutil.GetCalls(calls, mu), text.SlugHash("proxmox-repl", "pve1:100-0", 4),
				"Replication Job: '100-0' failed", "Replication Failed", pushward.ColorRed)
		})
	}
}

// A manual backup carries no job-id; it is keyed on the node, whatever guest
// the log names first.
func TestManualBackupKeysOnNode(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())
	for _, vmid := range []string{"100", "205"} {
		w := send(t, h, `{
			"type": "vzdump",
			"title": "vzdump backup status (pve1.home.arpa): backup successful",
			"message": "INFO: Starting Backup of VM `+vmid+` (qemu)",
			"severity": "info",
			"hostname": "pve1",
			"job_id": ""
		}`)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		time.Sleep(100 * time.Millisecond)
	}
	want := text.SlugHash("proxmox-backup", "pve1:", 4)
	creates := 0
	for _, c := range testutil.GetCalls(calls, mu) {
		if c.Method == "POST" {
			creates++
			var create pushward.CreateActivityRequest
			testutil.UnmarshalBody(t, c.Body, &create)
			if create.Slug != want {
				t.Errorf("expected slug %s, got %s", want, create.Slug)
			}
		}
	}
	if creates != 2 {
		t.Errorf("expected 2 creates, got %d", creates)
	}
}

// The severity decides the outcome; the title, which also names the node, only
// when a body sends no severity.
func TestBackupSeverityDecidesOverTitle(t *testing.T) {
	for _, tc := range []struct {
		name, title, severity, state, accent string
	}{
		{"node name with fail", "vzdump backup status (pve1.failover.lan): backup successful", "info", "Backup Complete", pushward.ColorGreen},
		{"no severity", "vzdump backup status (pve1): backup failed", "", "Backup Failed", pushward.ColorRed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, calls, mu := newHandler(t, testConfig())
			body, _ := json.Marshal(map[string]string{
				"type": "vzdump", "title": tc.title, "severity": tc.severity,
				"hostname": "pve1", "job_id": "backup-1",
			})
			if w := send(t, h, string(body)); w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			time.Sleep(100 * time.Millisecond)
			assertOpenedAndEnded(t, testutil.GetCalls(calls, mu),
				text.SlugHash("proxmox-backup", "pve1:backup-1", 4), tc.title, tc.state, tc.accent)
		})
	}
}

// PVE never sends a start, but a hook script posting a title with "start" in
// it opens a running card, and the outcome on the same key ends it.
func TestStartTitleOpensRunningCard(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"type": "vzdump",
		"title": "vzdump backup starting on pve1",
		"message": "",
		"severity": "info",
		"hostname": "pve1"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected create + ONGOING and no end, got %d calls", len(recorded))
	}
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing || update.Content.State != "Backing up..." || update.Content.AccentColor != pushward.ColorBlue {
		t.Errorf("expected ONGOING 'Backing up...' blue, got %s %q %s", update.State, update.Content.State, update.Content.AccentColor)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 1 || update.Content.TotalSteps == nil || *update.Content.TotalSteps != 2 {
		t.Errorf("expected step 1 of 2")
	}

	send(t, h, `{
		"type": "vzdump",
		"title": "vzdump backup status (pve1.home.arpa): backup successful",
		"message": "INFO: Starting Backup of VM 100 (qemu)",
		"severity": "info",
		"hostname": "pve1"
	}`)
	time.Sleep(100 * time.Millisecond)

	recorded = testutil.GetCalls(calls, mu)
	assertOpenedAndEnded(t, recorded[2:], text.SlugHash("proxmox-backup", "pve1:", 4),
		"vzdump backup status (pve1.home.arpa): backup successful", "Backup Complete", pushward.ColorGreen)
}

func TestFencingAlert(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"type": "fencing",
		"title": "Node pve2 fenced",
		"message": "Node pve2 has been fenced by HA manager",
		"severity": "error",
		"hostname": "pve2"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	// Verify create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "alert" {
		t.Errorf("expected template 'alert', got %s", update.Content.Template)
	}
	if update.Content.Icon != "exclamationmark.octagon.fill" {
		t.Errorf("expected icon exclamationmark.octagon.fill, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red accent, got %s", update.Content.AccentColor)
	}
	if update.Content.Subtitle != "Proxmox · pve2" {
		t.Errorf("expected subtitle 'Proxmox · pve2', got %q", update.Content.Subtitle)
	}
	// The badge names the event class; the state line is the raw title.
	if update.Content.SeverityLabel != "Fencing" {
		t.Errorf("expected severity_label 'Fencing', got %q", update.Content.SeverityLabel)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestUpdatesNotification(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"type": "package-updates",
		"title": "5 updates available",
		"message": "5 package updates available on pve1",
		"severity": "info",
		"hostname": "pve1"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "alert" {
		t.Errorf("expected template 'alert', got %s", update.Content.Template)
	}
	if update.Content.Icon != "arrow.down.circle" {
		t.Errorf("expected icon arrow.down.circle, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != pushward.ColorBlue {
		t.Errorf("expected blue accent, got %s", update.Content.AccentColor)
	}
	if update.Content.State != "5 updates available" {
		t.Errorf("expected state '5 updates available', got %s", update.Content.State)
	}
	if update.Content.SeverityLabel != "Updates" {
		t.Errorf("expected severity_label 'Updates', got %q", update.Content.SeverityLabel)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestSystemEventSendsTestNotification(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"type": "system",
		"title": "System event",
		"message": "Something happened",
		"severity": "info",
		"hostname": "pve1"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// selftest: create + update = 2 calls
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls for system event (selftest), got %d", len(recorded))
	}

	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if create.Slug != "relay-test-proxmox" {
		t.Errorf("expected slug relay-test-proxmox, got %s", create.Slug)
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "steps" {
		t.Errorf("expected template steps, got %s", update.Content.Template)
	}
}

// The "Test" button under Datacenter > Notifications fires a webhook with an
// empty type metadata field (verified against PVE 8.4); it must still produce
// the self-test activity so users can confirm the integration works.
func TestTestButtonSendsSelfTest(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"type": "",
		"title": "Test notification ",
		"message": "This is a test of the PushWard target.",
		"severity": "info",
		"hostname": ""
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls for empty-type test webhook (selftest), got %d", len(recorded))
	}

	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if create.Slug != "relay-test-proxmox" {
		t.Errorf("expected slug relay-test-proxmox, got %s", create.Slug)
	}
}

func TestSystemMailAlert(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"type": "system-mail",
		"title": "Certificate renewal failed",
		"message": "Could not renew ACME certificate for pve1",
		"severity": "error",
		"hostname": "pve1"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "alert" {
		t.Errorf("expected template 'alert', got %s", update.Content.Template)
	}
	if update.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red accent for error severity, got %s", update.Content.AccentColor)
	}
	if update.Content.Severity != "critical" {
		t.Errorf("expected severity 'critical' for error, got %s", update.Content.Severity)
	}
	// Proxmox has four levels and PushWard three, so the mapping folds notice
	// into info; the badge keeps the word Proxmox actually sent.
	if update.Content.SeverityLabel != "Error" {
		t.Errorf("expected severity_label 'Error', got %q", update.Content.SeverityLabel)
	}
	if update.Content.State != "Certificate renewal failed" {
		t.Errorf("expected state 'Certificate renewal failed', got %s", update.Content.State)
	}
	if update.Content.Subtitle != "Proxmox · pve1" {
		t.Errorf("expected subtitle 'Proxmox · pve1', got %q", update.Content.Subtitle)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

// system-mail is the catch-all type; distinct titles on one host must collapse
// to a single activity slug so a maintenance mail storm can't exhaust the
// server's concurrent-activity budget and evict in-progress backups.
func TestSystemMailCollapsesPerHost(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	send(t, h, `{
		"type": "system-mail",
		"title": "Certificate renewal failed",
		"severity": "error",
		"hostname": "pve1"
	}`)
	time.Sleep(50 * time.Millisecond)
	send(t, h, `{
		"type": "system-mail",
		"title": "SMART warning on /dev/sda",
		"severity": "warning",
		"hostname": "pve1"
	}`)
	time.Sleep(50 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	var slugs []string
	for _, c := range recorded {
		if c.Method == "POST" && c.Path == "/activities" {
			var cr pushward.CreateActivityRequest
			testutil.UnmarshalBody(t, c.Body, &cr)
			slugs = append(slugs, cr.Slug)
		}
	}
	if len(slugs) < 2 {
		t.Fatalf("expected 2 create calls, got %d", len(slugs))
	}
	if slugs[0] != slugs[1] {
		t.Errorf("expected one slug per host, got %q and %q", slugs[0], slugs[1])
	}
}

// A forwarded mail with no subject yields an empty title; the server requires a
// non-empty name, so the handler must fall back rather than POST an empty name.
func TestSystemMailEmptyTitleFallback(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"type": "system-mail",
		"title": "",
		"message": "ZFS pool degraded",
		"severity": "warning",
		"hostname": "pve1"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("expected create + update, got %d calls", len(recorded))
	}

	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if create.Name == "" {
		t.Error("expected a non-empty name fallback for an empty title")
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State == "" {
		t.Error("expected a non-empty alert state for an empty title")
	}
	if update.Content.AccentColor != pushward.ColorOrange {
		t.Errorf("expected orange accent for warning severity, got %s", update.Content.AccentColor)
	}
}
