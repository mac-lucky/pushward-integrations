package detect

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/relaytest"
)

// oodDir is where the classifier checkout keeps payloads from services with
// no relay provider. They are not vendored here. DETECT_OOD_DIR points
// elsewhere; set is true when it did, so a bad path is an error rather than
// a skip.
func oodDir() (dir string, set bool) {
	if d := os.Getenv("DETECT_OOD_DIR"); d != "" {
		return d, true
	}
	return filepath.Join("..", "..", "..", "..", "pushward-classifier", "dataset", "ood"), false
}

// oodHeaders are headers the OOD services send, as far as their docs say.
// The vendor event headers are the ones that matter: each must veto, or at
// least not claim, its payload.
var oodHeaders = map[string]http.Header{
	"alertmanager":   {"User-Agent": {"Alertmanager/0.27.0"}},
	"aws-cloudwatch": {"User-Agent": {"Amazon Simple Notification Service Agent"}, "X-Amz-Sns-Message-Type": {"Notification"}},
	"circleci":       {"User-Agent": {"CircleCI-Webhook/1.0"}, "Circleci-Event-Type": {"workflow-completed"}},
	"drone":          {"User-Agent": {"Go-http-client/1.1"}, "X-Drone-Event": {"build"}},
	"gcp-monitoring": {"User-Agent": {"Google-Alerts"}},
	"github":         {"User-Agent": {"GitHub-Hookshot/0000000"}, "X-Github-Event": {"workflow_job"}, "X-Github-Delivery": {"00000000-0000-4000-8000-000000000301"}},
	"gitlab":         {"User-Agent": {"GitLab/17.5.0"}, "X-Gitlab-Event": {"Pipeline Hook"}},
	"harbor":         {"User-Agent": {"Go-http-client/1.1"}},
	"pagerduty":      {"User-Agent": {"PagerDuty-Webhook/V3.0"}},
	"pingdom":        {"User-Agent": {"Pingdom.com_bot_version_1.4_(http://www.pingdom.com/)"}},
	"plex":           {"User-Agent": {"PlexMediaServer/1.41.0.8994"}},
	"sentry":         {"User-Agent": {"sentry"}, "Sentry-Hook-Resource": {"issue"}},
	"travis-ci":      {"User-Agent": {"Travis CI Notifications"}, "Travis-Repo-Slug": {"acme/app"}},
	"updown":         {"User-Agent": {"updown.io webhooks"}},
}

// labelled is one payload file: the classifier's OOD records and the
// negatives committed here share the source and payload keys.
type labelled struct {
	Source  string            `json:"source"`
	Headers map[string]string `json:"headers"`
	Payload json.RawMessage   `json:"payload"`
}

func readLabelled(t *testing.T, root string) map[string]labelled {
	t.Helper()
	out := map[string]labelled{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		b, err := os.ReadFile(path) // #nosec G304 G122 -- test payloads under a directory the test names
		if err != nil {
			return err
		}
		var l labelled
		if err := json.Unmarshal(b, &l); err != nil {
			return err
		}
		if len(l.Payload) == 0 {
			t.Errorf("%s has no payload", path)
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = l
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertNoMatch(t *testing.T, name string, l labelled) {
	t.Helper()
	h := http.Header{}
	for k, v := range oodHeaders[l.Source] {
		h[k] = v
	}
	for k, v := range l.Headers {
		h.Set(k, v)
	}
	if m, ok := Detect(http.Header{}, l.Payload); ok {
		t.Errorf("%s: body alone detected as %+v", name, m)
	}
	if m, ok := Detect(h, l.Payload); ok {
		t.Errorf("%s: with %s headers detected as %+v", name, l.Source, m)
	}
}

// TestOODNotDetected runs Detect over the classifier's out-of-distribution
// payloads, none of which comes from a relay provider.
func TestOODNotDetected(t *testing.T) {
	dir, set := oodDir()
	if _, err := os.Stat(dir); err != nil {
		if set {
			t.Fatalf("DETECT_OOD_DIR: %v", err)
		}
		t.Skipf("no OOD payloads at %s (set DETECT_OOD_DIR)", dir)
	}
	payloads := readLabelled(t, dir)
	if len(payloads) == 0 {
		t.Fatalf("no payloads under %s", dir)
	}
	for name, l := range payloads {
		assertNoMatch(t, name, l)
	}
	t.Logf("%d OOD payloads, none detected", len(payloads))
}

// TestNegativesNotDetected runs the committed hand-written negatives.
func TestNegativesNotDetected(t *testing.T) {
	payloads := readLabelled(t, filepath.Join("testdata", "negative"))
	if len(payloads) < 8 {
		t.Fatalf("%d negatives, want at least 8", len(payloads))
	}
	for name, l := range payloads {
		assertNoMatch(t, name, l)
	}
}

// TestLookalikes sends real provider fixtures with a sender's headers that
// say they came from elsewhere.
func TestLookalikes(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		h       http.Header
		rule    string
	}{
		{"radarr payload from Lidarr", "radarr/grab.json", hdr("User-Agent", "Lidarr/2.5.3.4341"), "starr-other"},
		{"gitea run with only X-GitHub-Event", "gitea/run_requested.json", hdr("X-GitHub-Event", "workflow_run"), "github"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := Detect(tt.h, relaytest.ReadFixture(t, filepath.Join(relaytest.Testdata, tt.fixture)))
			if ok || m.Via != ViaVeto || m.Rule != tt.rule {
				t.Errorf("Detect = %+v, %v; want a %s veto", m, ok, tt.rule)
			}
		})
	}
}
