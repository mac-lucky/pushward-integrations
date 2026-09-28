// Package relaytest drives relay/testdata fixtures through the relay's real
// handlers and records the PushWard calls they make. It is imported by tests
// only: the fixture exporter, the root route's dispatch test and the
// universal route's scorecard all replay the same fixtures the same way.
package relaytest

import (
	"bytes"
	"encoding/base64"
	"path/filepath"
	"runtime"
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
	"github.com/mac-lucky/pushward-integrations/relay/internal/jellyfin"
	"github.com/mac-lucky/pushward-integrations/relay/internal/komodo"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overseerr"
	"github.com/mac-lucky/pushward-integrations/relay/internal/paperless"
	"github.com/mac-lucky/pushward-integrations/relay/internal/proxmox"
	"github.com/mac-lucky/pushward-integrations/relay/internal/starr"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/truenas"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universalhook"
	"github.com/mac-lucky/pushward-integrations/relay/internal/unmanic"
	"github.com/mac-lucky/pushward-integrations/relay/internal/uptimekuma"
	"github.com/mac-lucky/pushward-integrations/shared/poster"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// Testdata is the path of relay/testdata. It is found from this file's own
// location, so it holds whatever directory the importing test runs in.
var Testdata = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata")
}()

// Key is the integration key every replayed request carries.
const Key = "hlk_test"

// Base mirrors the providers' own handler-test configs: ends fire within
// milliseconds so the two-phase end is part of the recorded calls.
func Base() config.BaseProviderConfig {
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

// UniversalConfig is the universal route's config for tests: Base plus a
// public URL and a fixed review key.
func UniversalConfig() *config.UniversalConfig {
	return &config.UniversalConfig{
		BaseProviderConfig: Base(),
		PublicURL:          "https://relay.example.com",
		ReviewKey:          base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)),
	}
}

// Register adds a provider's routes to api and returns its handler, so the
// caller can stop its Ender.
type Register func(t *testing.T, api huma.API, store state.Store, pool *client.Pool) any

// Route is one webhook path and the fixture directory that feeds it.
type Route struct {
	Provider string
	Path     string
	Dir      string
	Register Register
}

// Routes returns a route per fixture directory of the dedicated providers, in
// the order the exporter writes them. The 16 providers are the ones wired in
// cmd/pushward-relay; starr serves three directories and gitea two.
// testdata/universal is the universal route's own; see Universal.
func Routes() []Route {
	posters := poster.Disabled{}
	starrRoutes := func(_ *testing.T, api huma.API, store state.Store, pool *client.Pool) any {
		return starr.RegisterRoutes(api, store, pool, &config.StarrConfig{BaseProviderConfig: Base()}, posters)
	}
	giteaRoutes := func(_ *testing.T, api huma.API, store state.Store, pool *client.Pool) any {
		return gitea.RegisterRoutes(api, store, pool, &config.GiteaConfig{BaseProviderConfig: Base()})
	}
	return []Route{
		{"grafana", "/grafana", "grafana", func(_ *testing.T, api huma.API, store state.Store, pool *client.Pool) any {
			grafana.RegisterRoutes(api, store, pool, &config.GrafanaConfig{BaseProviderConfig: Base()})
			return nil
		}},
		{"argocd", "/argocd", "argocd", func(_ *testing.T, api huma.API, store state.Store, pool *client.Pool) any {
			return argocd.RegisterRoutes(api, store, pool, &config.ArgoCDConfig{BaseProviderConfig: Base()})
		}},
		{"starr", "/radarr", "radarr", starrRoutes},
		{"starr", "/sonarr", "sonarr", starrRoutes},
		{"starr", "/prowlarr", "prowlarr", starrRoutes},
		{"jellyfin", "/jellyfin", "jellyfin", func(_ *testing.T, api huma.API, store state.Store, pool *client.Pool) any {
			return jellyfin.RegisterRoutes(api, store, pool, &config.JellyfinConfig{
				BaseProviderConfig: Base(),
				ProgressDebounce:   10 * time.Millisecond,
				PauseTimeout:       time.Hour,
			}, posters)
		}},
		{"paperless", "/paperless", "paperless", func(_ *testing.T, api huma.API, store state.Store, pool *client.Pool) any {
			return paperless.RegisterRoutes(api, store, pool, &config.PaperlessConfig{BaseProviderConfig: Base()})
		}},
		{"changedetection", "/changedetection", "changedetection", func(_ *testing.T, api huma.API, _ state.Store, pool *client.Pool) any {
			changedetection.RegisterRoutes(api, pool, &config.ChangedetectionConfig{BaseProviderConfig: Base()})
			return nil
		}},
		{"unmanic", "/unmanic", "unmanic", func(_ *testing.T, api huma.API, _ state.Store, pool *client.Pool) any {
			return unmanic.RegisterRoutes(api, pool, &config.UnmanicConfig{BaseProviderConfig: Base()})
		}},
		{"bazarr", "/bazarr", "bazarr", func(_ *testing.T, api huma.API, _ state.Store, pool *client.Pool) any {
			return bazarr.RegisterRoutes(api, pool, &config.BazarrConfig{BaseProviderConfig: Base()})
		}},
		{"proxmox", "/proxmox", "proxmox", func(_ *testing.T, api huma.API, store state.Store, pool *client.Pool) any {
			return proxmox.RegisterRoutes(api, store, pool, &config.ProxmoxConfig{BaseProviderConfig: Base()})
		}},
		{"overseerr", "/overseerr", "overseerr", func(_ *testing.T, api huma.API, store state.Store, pool *client.Pool) any {
			return overseerr.RegisterRoutes(api, store, pool, &config.OverseerrConfig{BaseProviderConfig: Base()}, posters)
		}},
		{"uptimekuma", "/uptimekuma", "uptimekuma", func(_ *testing.T, api huma.API, store state.Store, pool *client.Pool) any {
			return uptimekuma.RegisterRoutes(api, store, pool, &config.UptimeKumaConfig{BaseProviderConfig: Base()})
		}},
		{"gatus", "/gatus", "gatus", func(_ *testing.T, api huma.API, store state.Store, pool *client.Pool) any {
			return gatus.RegisterRoutes(api, store, pool, &config.GatusConfig{BaseProviderConfig: Base()})
		}},
		{"backrest", "/backrest", "backrest", func(_ *testing.T, api huma.API, store state.Store, pool *client.Pool) any {
			return backrest.RegisterRoutes(api, store, pool, &config.BackrestConfig{BaseProviderConfig: Base()})
		}},
		{"gitea", "/gitea", "gitea", giteaRoutes},
		{"gitea", "/forgejo", "forgejo", giteaRoutes},
		{"komodo", "/komodo", "komodo", func(_ *testing.T, api huma.API, store state.Store, pool *client.Pool) any {
			return komodo.RegisterRoutes(api, store, pool, &config.KomodoConfig{BaseProviderConfig: Base()})
		}},
		{"truenas", "/truenas/v2/alerts", "truenas", func(_ *testing.T, api huma.API, store state.Store, pool *client.Pool) any {
			return truenas.RegisterRoutes(api, store, pool, &config.TrueNASConfig{BaseProviderConfig: Base()})
		}},
	}
}

// Universal is the universal route fed from fixture directory dir. A
// non-empty source goes out as ?source=, the way a sender names itself.
func Universal(dir, source string) Route {
	path := "/universal"
	if source != "" {
		path += "?source=" + source
	}
	return Route{Provider: "universal", Path: path, Dir: dir, Register: registerUniversal}
}

// registerUniversal registers the universal route the way main does: its
// store hashes keys strictly, and its mappings live in memory.
func registerUniversal(t *testing.T, api huma.API, store state.Store, pool *client.Pool) any {
	t.Helper()
	h, err := universalhook.RegisterRoutes(api, state.KeyHashing(store, state.KeyModeStrict),
		state.NewMemoryMappingStore(), pool, UniversalConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return h
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

// Prime returns the fixture ("<dir>/<file>") that has to go first, or "".
func Prime(fixture string) string {
	return primes[fixture]
}
