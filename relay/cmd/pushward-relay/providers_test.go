package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universal/ranker"
	"github.com/mac-lucky/pushward-integrations/shared/poster"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

const (
	testdata = "../../testdata"
	testKey  = "hlk_providers_test"
)

// fixtureRoutes maps each relay/testdata directory to the route its payloads
// are posted to.
var fixtureRoutes = map[string]string{
	"argocd":          "/argocd",
	"backrest":        "/backrest",
	"bazarr":          "/bazarr",
	"changedetection": "/changedetection",
	"forgejo":         "/forgejo",
	"gatus":           "/gatus",
	"gitea":           "/gitea",
	"grafana":         "/grafana",
	"jellyfin":        "/jellyfin",
	"komodo":          "/komodo",
	"overseerr":       "/overseerr",
	"paperless":       "/paperless",
	"prowlarr":        "/prowlarr",
	"proxmox":         "/proxmox",
	"radarr":          "/radarr",
	"sonarr":          "/sonarr",
	"truenas":         "/truenas/v2/alerts",
	"universal":       "/universal",
	"unmanic":         "/unmanic",
	"uptimekuma":      "/uptimekuma",
}

// ackRoutes maps a fixture directory to a second route that runs the same
// handlers with ?ack=1 turned on by the path.
var ackRoutes = map[string]string{
	"truenas": "/truenas/ack/v2/alerts",
}

// stateful are the providers whose fixtures leave a relay_state row behind.
// changedetection, unmanic and bazarr are handed no store at all, and the
// universal route opens no card for a payload it does not know.
var stateful = []string{
	"argocd", "backrest", "gatus", "gitea", "grafana", "jellyfin", "komodo",
	"overseerr", "paperless", "proxmox", "starr", "truenas", "uptimekuma",
}

// loadConfig returns the production defaults with every provider on.
func loadConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("PUSHWARD_DATABASE_DSN", "postgres://relay@localhost/relay")
	for _, name := range []string{"GRAFANA", "ARGOCD", "STARR", "GITEA", "UNIVERSAL"} {
		t.Setenv("PUSHWARD_"+name+"_ENABLED", "true")
	}
	t.Setenv("PUSHWARD_STARR_MODE", "")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// TestRegisterProviders_HashesKeys posts every fixture to its route through
// the store registerProviders hands out, then checks that each row was
// written under the hashed tenant key and that no row carries the raw one.
func TestRegisterProviders_HashesKeys(t *testing.T) {
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, _, _ := testutil.MockPushWardServer(t)
	mem := state.NewMemoryStore()
	mux, api := humautil.NewTestAPI()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	r := registerProviders(ctx, api, mem, client.NewPool(srv.URL, nil), loadConfig(t), poster.Disabled{})
	t.Cleanup(func() {
		r.argocd.StopAll()
		for _, e := range r.enders {
			e.StopAll()
		}
	})

	// Every registered webhook route is exercised, and every fixture
	// directory has a route.
	routed := make(map[string]bool, len(fixtureRoutes))
	for _, path := range fixtureRoutes {
		routed[path] = true
	}
	for _, path := range ackRoutes {
		routed[path] = true
	}
	for path, item := range api.OpenAPI().Paths {
		if item.Post != nil && !routed[path] {
			t.Errorf("POST %s has no fixture directory in fixtureRoutes", path)
		}
	}
	dirs, err := os.ReadDir(testdata)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if _, ok := fixtureRoutes[d.Name()]; d.IsDir() && !ok {
			t.Errorf("testdata/%s has no route in fixtureRoutes", d.Name())
		}
	}

	dirNames := make([]string, 0, len(fixtureRoutes))
	for dir := range fixtureRoutes {
		dirNames = append(dirNames, dir)
	}
	slices.Sort(dirNames)
	for _, dir := range dirNames {
		names, err := filepath.Glob(filepath.Join(testdata, dir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(names) == 0 {
			t.Errorf("testdata/%s has no fixtures", dir)
		}
		for _, name := range names {
			body, err := os.ReadFile(name) // #nosec G304 -- fixture path under relay/testdata
			if err != nil {
				t.Fatal(err)
			}
			paths := []string{fixtureRoutes[dir]}
			if p, ok := ackRoutes[dir]; ok {
				paths = append(paths, p)
			}
			for _, path := range paths {
				if resp := post(mux, path, body); resp.Code != http.StatusOK {
					t.Errorf("POST %s %s/%s: %d %s", path, dir, filepath.Base(name), resp.Code, resp.Body.String())
				}
			}
		}
	}

	want := state.HashKey(testKey)
	perProvider := map[string]int{}
	for _, row := range mem.Rows() {
		if row.UserKey != want {
			t.Errorf("%s row %s/%s stored under %q, want %q", row.Provider, row.Key, row.SubKey, row.UserKey, want)
		}
		// The key must not leak into the rest of the row either.
		if strings.Contains(row.Key, testKey) || strings.Contains(row.SubKey, testKey) || bytes.Contains(row.Value, []byte(testKey)) {
			t.Errorf("%s row %s/%s carries the raw key outside user_key", row.Provider, row.Key, row.SubKey)
		}
		perProvider[row.Provider]++
	}
	for _, p := range stateful {
		if perProvider[p] == 0 {
			t.Errorf("no %s rows: the fixtures no longer exercise its store", p)
		}
	}
}

func post(h http.Handler, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	return resp
}

// The ranker leads only when its embedded weights passed their gate.
func TestUniversalRankerFollowsItsGate(t *testing.T) {
	_, pass, err := ranker.Info()
	want := err == nil && pass
	if got := universalRanker() != nil; got != want {
		t.Errorf("universalRanker() enabled = %v, want %v (pass %v, err %v)", got, want, pass, err)
	}
}
