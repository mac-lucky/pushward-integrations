package detect

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/relaytest"
)

// headerOnly are the fixture directories whose sender is known by its headers
// alone; no body rule may claim their payloads.
var headerOnly = map[string]bool{"radarr": true, "sonarr": true, "prowlarr": true, "gitea": true, "forgejo": true}

// undetected are fixtures that must go to the universal route: TrueNAS speaks
// OpsGenie's protocol, which other senders speak too, and a Gitea push is not
// an Actions run.
var undetected = map[string]bool{
	"truenas/create.json":     true,
	"truenas/test_alert.json": true,
	"gitea/push_ignored.json": true,
}

func fixtureDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(relaytest.Testdata)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

// TestFixturesBodyRules checks every fixture against every body rule: a
// provider's fixtures match its own rule and no other, and everyone else's
// match none.
func TestFixturesBodyRules(t *testing.T) {
	byDir := map[string]string{}
	for _, rt := range relaytest.Routes() {
		byDir[rt.Dir] = rt.Path
	}
	for _, dir := range fixtureDirs(t) {
		names, err := filepath.Glob(filepath.Join(relaytest.Testdata, dir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			fixture := dir + "/" + filepath.Base(name)
			t.Run(fixture, func(t *testing.T) {
				v := &view{body: relaytest.ReadFixture(t, name)}
				var matched []string
				for _, r := range bodyRules {
					if r.match(v) {
						matched = append(matched, r.route)
					}
				}
				want := byDir[dir]
				switch {
				case len(matched) > 1:
					t.Errorf("matches %d body rules: %v", len(matched), matched)
				case headerOnly[dir] || dir == "truenas" || dir == "universal":
					if len(matched) != 0 {
						t.Errorf("matches body rule %v, want none", matched)
					}
				case len(matched) == 0 || matched[0] != want:
					t.Errorf("body rules match %v, want %s", matched, want)
				}
			})
		}
	}
}

// TestFixturesWithSenderHeaders runs Detect as the root route does, with the
// headers the real sender adds.
func TestFixturesWithSenderHeaders(t *testing.T) {
	for _, rt := range relaytest.Routes() {
		names, err := filepath.Glob(filepath.Join(relaytest.Testdata, rt.Dir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			fixture := rt.Dir + "/" + filepath.Base(name)
			t.Run(fixture, func(t *testing.T) {
				body := relaytest.ReadFixture(t, name)
				m, ok := Detect(relaytest.SenderHeaders(rt.Dir, body), body)
				if undetected[fixture] {
					if ok {
						t.Errorf("Detect = %+v, want no match", m)
					}
					return
				}
				wantVia := ViaBody
				if headerOnly[rt.Dir] {
					wantVia = ViaHeader
				}
				if !ok || m.Route != rt.Path || m.Via != wantVia {
					t.Errorf("Detect = %+v, %v; want %s via %s", m, ok, rt.Path, wantVia)
				}
			})
		}
	}
}

// TestRoutesRegistered checks that every route Detect returns is one the
// relay serves.
func TestRoutesRegistered(t *testing.T) {
	h := relaytest.Relay(t)
	for _, route := range Routes() {
		_, pattern := h.Mux.Handler(&http.Request{Method: http.MethodPost, URL: &url.URL{Path: route}})
		if pattern != "POST "+route {
			t.Errorf("POST %s is served by pattern %q", route, pattern)
		}
	}
}
