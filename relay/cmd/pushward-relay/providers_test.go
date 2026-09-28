package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
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
	"proxmox":         "/proxmox",
	"radarr":          "/radarr",
	"sonarr":          "/sonarr",
	"truenas":         "/truenas/v2/alerts",
	"universal":       "/universal",
	"unmanic":         "/unmanic",
	"uptimekuma":      "/uptimekuma",
}

// noFixtures are registered POST routes with no testdata directory yet.
// Prowlarr is notification-only and keeps no state.
var noFixtures = map[string]bool{"/prowlarr": true}

// stateful are the providers whose fixtures leave a relay_state row behind.
// changedetection, unmanic and bazarr are handed no store at all.
var stateful = []string{
	"argocd", "backrest", "gatus", "gitea", "grafana", "jellyfin", "komodo",
	"overseerr", "paperless", "proxmox", "starr", "truenas", "universal", "uptimekuma",
}

// loadConfig returns the production defaults, every provider on, with the
// given key mode.
func loadConfig(t *testing.T, keyMode string) *config.Config {
	t.Helper()
	t.Setenv("PUSHWARD_DATABASE_DSN", "postgres://relay@localhost/relay")
	t.Setenv("PUSHWARD_STATE_KEY_MODE", keyMode)
	for _, name := range []string{"GRAFANA", "ARGOCD", "STARR", "GITEA", "UNIVERSAL"} {
		t.Setenv("PUSHWARD_"+name+"_ENABLED", "true")
	}
	t.Setenv("PUSHWARD_UNIVERSAL_PUBLIC_URL", "https://relay.example.com")
	t.Setenv("PUSHWARD_UNIVERSAL_REVIEW_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	t.Setenv("PUSHWARD_UNIVERSAL_REVIEW_KEY_FILE", "")
	t.Setenv("PUSHWARD_STARR_MODE", "")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// TestRegisterProviders_KeyMode posts every fixture to its route through the
// store registerProviders hands out, then checks the tenant key each row was
// written under. In hashed mode no row may carry the raw hlk_ key; the
// universal route hashes in every mode.
func TestRegisterProviders_KeyMode(t *testing.T) {
	tests := []struct {
		mode    string
		wantKey string
	}{
		{config.KeyModeHashed, state.HashKey(testKey)},
		{config.KeyModeCompat, testKey},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			lifecycle.SetRetryDelay(10 * time.Millisecond)
			srv, _, _ := testutil.MockPushWardServer(t)
			mem := state.NewMemoryStore()
			mappings := state.NewMemoryMappingStore()
			mux, api := humautil.NewTestAPI()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			r, err := registerProviders(ctx, api, mem, mappings, client.NewPool(srv.URL, nil), loadConfig(t, tt.mode), poster.Disabled{})
			if err != nil {
				t.Fatal(err)
			}
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
			for path, item := range api.OpenAPI().Paths {
				if item.Post != nil && !routed[path] && !noFixtures[path] {
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
					if resp := post(mux, fixtureRoutes[dir], body); resp.Code != http.StatusOK {
						t.Errorf("POST %s %s/%s: %d %s", fixtureRoutes[dir], dir, filepath.Base(name), resp.Code, resp.Body.String())
					}
				}
			}

			perProvider := map[string]int{}
			for _, row := range mem.Rows() {
				want := tt.wantKey
				if row.Provider == "universal" {
					want = state.HashKey(testKey)
				}
				if want != testKey && strings.HasPrefix(row.UserKey, "hlk_") {
					t.Errorf("%s row %s/%s stored the raw key", row.Provider, row.Key, row.SubKey)
				}
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
			rows := mappings.Rows()
			if len(rows) == 0 {
				t.Error("no universal mappings: the fixtures no longer exercise the mapping store")
			}
			for _, row := range rows {
				if row.KeyHash != auth.UniversalDigest(testKey) {
					t.Errorf("mapping stored under key hash %x, want the universal digest of the test key", row.KeyHash[:4])
				}
				for _, col := range [][]byte{row.Mapping, row.Shape, row.Proposal, row.Samples, row.Candidates} {
					if bytes.Contains(col, []byte("hlk_")) {
						t.Errorf("mapping row carries a raw key: %s", col)
					}
				}
			}
		})
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

// The config package validates key_mode itself; each value it accepts has to
// be one state.KeyHashing knows, or registerProviders would panic at boot.
func TestConfigKeyModesAreStateModes(t *testing.T) {
	for _, m := range []string{config.KeyModeCompat, config.KeyModeHashed} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("config key mode %q is not a state.KeyMode: %v", m, r)
				}
			}()
			state.KeyHashing(state.NewMemoryStore(), state.KeyMode(m))
		}()
	}
}
