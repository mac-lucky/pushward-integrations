//go:build fixtureexport

package dataset

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/argocd"
	"github.com/mac-lucky/pushward-integrations/relay/internal/backrest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/bazarr"
	"github.com/mac-lucky/pushward-integrations/relay/internal/changedetection"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/gatus"
	"github.com/mac-lucky/pushward-integrations/relay/internal/gitea"
	"github.com/mac-lucky/pushward-integrations/relay/internal/grafana"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/jellyfin"
	"github.com/mac-lucky/pushward-integrations/relay/internal/komodo"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overseerr"
	"github.com/mac-lucky/pushward-integrations/relay/internal/paperless"
	"github.com/mac-lucky/pushward-integrations/relay/internal/proxmox"
	"github.com/mac-lucky/pushward-integrations/relay/internal/starr"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/truenas"
	"github.com/mac-lucky/pushward-integrations/relay/internal/unmanic"
	"github.com/mac-lucky/pushward-integrations/relay/internal/uptimekuma"
	"github.com/mac-lucky/pushward-integrations/shared/poster"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

const testdata = "../../../testdata"

// base mirrors the providers' own handler-test configs: ends fire within
// milliseconds so the two-phase end is part of the recorded calls.
func base() config.BaseProviderConfig {
	return config.BaseProviderConfig{
		Enabled:        true,
		Priority:       2,
		CleanupDelay:   time.Hour,
		StaleTimeout:   30 * time.Minute,
		EndDelay:       10 * time.Millisecond,
		EndDisplayTime: 10 * time.Millisecond,
		DismissalDelay: pushward.DurationPtr(2 * time.Minute),
	}
}

type register func(api huma.API, store state.Store, pool *client.Pool) any

// route is one webhook path and the fixture directory that feeds it. The 16
// providers are the ones wired in cmd/pushward-relay/main.go; starr and gitea
// each serve two fixture directories.
type route struct {
	provider string
	path     string
	dir      string
	register register
}

func routes() []route {
	posters := poster.Disabled{}
	starrRoutes := func(api huma.API, store state.Store, pool *client.Pool) any {
		return starr.RegisterRoutes(api, store, pool, &config.StarrConfig{BaseProviderConfig: base()}, posters)
	}
	giteaRoutes := func(api huma.API, store state.Store, pool *client.Pool) any {
		return gitea.RegisterRoutes(api, store, pool, &config.GiteaConfig{BaseProviderConfig: base()})
	}
	return []route{
		{"grafana", "/grafana", "grafana", func(api huma.API, store state.Store, pool *client.Pool) any {
			grafana.RegisterRoutes(api, store, pool, &config.GrafanaConfig{BaseProviderConfig: base()})
			return nil
		}},
		{"argocd", "/argocd", "argocd", func(api huma.API, store state.Store, pool *client.Pool) any {
			return argocd.RegisterRoutes(api, store, pool, &config.ArgoCDConfig{BaseProviderConfig: base()})
		}},
		{"starr", "/radarr", "radarr", starrRoutes},
		{"starr", "/sonarr", "sonarr", starrRoutes},
		{"jellyfin", "/jellyfin", "jellyfin", func(api huma.API, store state.Store, pool *client.Pool) any {
			return jellyfin.RegisterRoutes(api, store, pool, &config.JellyfinConfig{
				BaseProviderConfig: base(),
				ProgressDebounce:   10 * time.Millisecond,
				PauseTimeout:       time.Hour,
			}, posters)
		}},
		{"paperless", "/paperless", "paperless", func(api huma.API, store state.Store, pool *client.Pool) any {
			return paperless.RegisterRoutes(api, store, pool, &config.PaperlessConfig{BaseProviderConfig: base()})
		}},
		{"changedetection", "/changedetection", "changedetection", func(api huma.API, _ state.Store, pool *client.Pool) any {
			changedetection.RegisterRoutes(api, pool, &config.ChangedetectionConfig{BaseProviderConfig: base()})
			return nil
		}},
		{"unmanic", "/unmanic", "unmanic", func(api huma.API, _ state.Store, pool *client.Pool) any {
			return unmanic.RegisterRoutes(api, pool, &config.UnmanicConfig{BaseProviderConfig: base()})
		}},
		{"bazarr", "/bazarr", "bazarr", func(api huma.API, _ state.Store, pool *client.Pool) any {
			return bazarr.RegisterRoutes(api, pool, &config.BazarrConfig{BaseProviderConfig: base()})
		}},
		{"proxmox", "/proxmox", "proxmox", func(api huma.API, store state.Store, pool *client.Pool) any {
			return proxmox.RegisterRoutes(api, store, pool, &config.ProxmoxConfig{BaseProviderConfig: base()})
		}},
		{"overseerr", "/overseerr", "overseerr", func(api huma.API, store state.Store, pool *client.Pool) any {
			return overseerr.RegisterRoutes(api, store, pool, &config.OverseerrConfig{BaseProviderConfig: base()}, posters)
		}},
		{"uptimekuma", "/uptimekuma", "uptimekuma", func(api huma.API, store state.Store, pool *client.Pool) any {
			return uptimekuma.RegisterRoutes(api, store, pool, &config.UptimeKumaConfig{BaseProviderConfig: base()})
		}},
		{"gatus", "/gatus", "gatus", func(api huma.API, store state.Store, pool *client.Pool) any {
			return gatus.RegisterRoutes(api, store, pool, &config.GatusConfig{BaseProviderConfig: base()})
		}},
		{"backrest", "/backrest", "backrest", func(api huma.API, store state.Store, pool *client.Pool) any {
			return backrest.RegisterRoutes(api, store, pool, &config.BackrestConfig{BaseProviderConfig: base()})
		}},
		{"gitea", "/gitea", "gitea", giteaRoutes},
		{"gitea", "/forgejo", "forgejo", giteaRoutes},
		{"komodo", "/komodo", "komodo", func(api huma.API, store state.Store, pool *client.Pool) any {
			return komodo.RegisterRoutes(api, store, pool, &config.KomodoConfig{BaseProviderConfig: base()})
		}},
		{"truenas", "/truenas/v2/alerts", "truenas", func(api huma.API, store state.Store, pool *client.Pool) any {
			return truenas.RegisterRoutes(api, store, pool, &config.TrueNASConfig{BaseProviderConfig: base()})
		}},
	}
}

// primes names the fixture that has to reach a handler before the one keyed
// here does anything: a resolve or finish event for a subject the handler
// never saw is dropped. Only the calls made after the prime are recorded.
var primes = map[string]string{
	"proxmox/backup_failure.json":             "proxmox/backup_start.json",
	"proxmox/backup_success.json":             "proxmox/backup_start.json",
	"uptimekuma/up.json":                      "uptimekuma/down.json",
	"gatus/resolved.json":                     "gatus/triggered.json",
	"komodo/server_unreachable_resolved.json": "komodo/server_unreachable.json",
}

type call struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

type record struct {
	Provider string          `json:"provider"`
	Route    string          `json:"route"`
	Fixture  string          `json:"fixture"`
	Primed   string          `json:"primed,omitempty"`
	Status   int             `json:"status"`
	Response string          `json:"response,omitempty"`
	Payload  json.RawMessage `json:"payload"`
	Calls    []call          `json:"calls"`
}

// TestExportFixtures sends each fixture to a fresh handler, as the providers'
// TestFixturesAccepted tests do, and writes what came in and what went out.
func TestExportFixtures(t *testing.T) {
	out := os.Getenv("UNIVERSAL_EXPORT_OUT")
	if out == "" {
		t.Skip("UNIVERSAL_EXPORT_OUT not set")
	}
	lifecycle.SetRetryDelay(10 * time.Millisecond)

	f, err := os.Create(out) // #nosec G304 G703 -- path chosen by whoever runs the export
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	seen := map[string]bool{}
	total := 0
	for _, rt := range routes() {
		seen[rt.dir] = true
		names, err := filepath.Glob(filepath.Join(testdata, rt.dir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(names)
		for _, name := range names {
			t.Run(rt.dir+"/"+filepath.Base(name), func(t *testing.T) {
				rec := exportOne(t, rt, name)
				if err := enc.Encode(rec); err != nil {
					t.Fatal(err)
				}
				total++
			})
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	// A fixture directory no route claims would silently drop out of the set.
	dirs, err := os.ReadDir(testdata)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if d.IsDir() && !seen[d.Name()] {
			t.Errorf("testdata/%s has no route in the exporter", d.Name())
		}
	}
	t.Logf("exported %d fixtures to %s", total, out)
}

func exportOne(t *testing.T, rt route, name string) record {
	t.Helper()
	payload, err := os.ReadFile(name) // #nosec G304 -- fixture path under relay/testdata
	if err != nil {
		t.Fatal(err)
	}
	srv, calls, mu := testutil.MockPushWardServer(t)
	mux, api := humautil.NewTestAPI()
	h := rt.register(api, state.NewMemoryStore(), client.NewPool(srv.URL, nil))
	if ep, ok := h.(lifecycle.EnderProvider); ok {
		t.Cleanup(ep.Ender().StopAll)
	}

	fixture := rt.dir + "/" + filepath.Base(name)
	skip := 0
	prime := primes[fixture]
	if prime != "" {
		body, err := os.ReadFile(filepath.Join(testdata, prime)) // #nosec G304 -- fixture path under relay/testdata
		if err != nil {
			t.Fatal(err)
		}
		if resp := post(mux, rt.path, body); resp.Code != http.StatusOK {
			t.Fatalf("prime %s: %d %s", prime, resp.Code, resp.Body.String())
		}
		skip = len(settle(calls, mu))
	}

	resp := post(mux, rt.path, payload)
	if resp.Code != http.StatusOK {
		t.Errorf("POST %s: %d %s", rt.path, resp.Code, resp.Body.String())
	}

	recorded := settle(calls, mu)[skip:]
	rec := record{
		Provider: rt.provider,
		Route:    rt.path,
		Fixture:  fixture,
		Primed:   prime,
		Status:   resp.Code,
		Payload:  json.RawMessage(compact(t, payload)),
		Calls:    make([]call, 0, len(recorded)),
	}
	if resp.Code != http.StatusOK {
		rec.Response = resp.Body.String()
	}
	for _, c := range recorded {
		rec.Calls = append(rec.Calls, call{Method: c.Method, Path: c.Path, Body: c.Body})
	}
	return rec
}

func post(h http.Handler, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	return resp
}

// settle waits for the delayed two-phase end: the call list has to stop
// growing for a while before it counts as complete.
func settle(calls *[]testutil.APICall, mu *sync.Mutex) []testutil.APICall {
	const quiet = 150 * time.Millisecond
	deadline := time.Now().Add(3 * time.Second)
	last, since := -1, time.Now()
	for time.Now().Before(deadline) {
		n := len(testutil.GetCalls(calls, mu))
		if n != last {
			last, since = n, time.Now()
		} else if time.Since(since) >= quiet {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return testutil.GetCalls(calls, mu)
}

func compact(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return buf.Bytes()
}
