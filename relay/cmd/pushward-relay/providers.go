package main

import (
	"context"
	"errors"
	"log/slog"

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
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overseerr"
	"github.com/mac-lucky/pushward-integrations/relay/internal/paperless"
	"github.com/mac-lucky/pushward-integrations/relay/internal/proxmox"
	"github.com/mac-lucky/pushward-integrations/relay/internal/starr"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/truenas"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universalhook"
	"github.com/mac-lucky/pushward-integrations/relay/internal/unmanic"
	"github.com/mac-lucky/pushward-integrations/relay/internal/uptimekuma"
	"github.com/mac-lucky/pushward-integrations/shared/poster"
)

// registered is what main needs back from the providers: the enders to flush
// and the ArgoCD handler whose grace timers stop first, for shutdown, and the
// universal handler, whose editor pages main serves on the mux.
type registered struct {
	enders    []*lifecycle.Ender
	argocd    *argocd.Handler
	universal *universalhook.Handler
}

// registerProviders registers every enabled provider on api. store is wrapped
// in KeyHashing once, here, so no provider can write a raw hlk_ key to
// relay_state; the periodic Cleanup keeps using the store main holds. The
// universal route gets its own strict wrapper: its rows never existed under a
// raw key, so it has no twin to read or clean up. mappings may be nil when the
// universal route is off.
func registerProviders(ctx context.Context, api huma.API, store state.Store, mappings state.MappingStore, clients *client.Pool, cfg *config.Config, posters poster.Source) (registered, error) {
	raw := store
	store = state.KeyHashing(store, state.KeyMode(cfg.State.KeyMode))
	slog.Info("state key mode", "mode", cfg.State.KeyMode)

	var r registered

	// collectEnder appends the handler's Ender if it implements lifecycle.EnderProvider.
	collectEnder := func(handler any) {
		if ep, ok := handler.(lifecycle.EnderProvider); ok {
			r.enders = append(r.enders, ep.Ender())
		}
	}

	if cfg.Providers.Grafana.Enabled {
		grafana.RegisterRoutes(api, store, clients, &cfg.Providers.Grafana)
		slog.Info("enabled provider", "provider", "grafana")
	}

	if cfg.Providers.ArgoCD.Enabled {
		ah := argocd.RegisterRoutes(api, store, clients, &cfg.Providers.ArgoCD)
		r.argocd = ah
		collectEnder(ah)
		ah.StartCleanup(ctx)
		slog.Info("enabled provider", "provider", "argocd")
	}

	if cfg.Providers.Starr.Enabled {
		sh := starr.RegisterRoutes(api, store, clients, &cfg.Providers.Starr, posters)
		collectEnder(sh)
		slog.Info("enabled provider", "provider", "starr")
	}

	if cfg.Providers.Jellyfin.Enabled {
		jh := jellyfin.RegisterRoutes(api, store, clients, &cfg.Providers.Jellyfin, posters)
		collectEnder(jh)
		jh.StartCleanup(ctx)
		slog.Info("enabled provider", "provider", "jellyfin")
	}

	if cfg.Providers.Paperless.Enabled {
		ph := paperless.RegisterRoutes(api, store, clients, &cfg.Providers.Paperless)
		collectEnder(ph)
		slog.Info("enabled provider", "provider", "paperless")
	}

	if cfg.Providers.Changedetection.Enabled {
		changedetection.RegisterRoutes(api, clients, &cfg.Providers.Changedetection)
		slog.Info("enabled provider", "provider", "changedetection")
	}

	if cfg.Providers.Unmanic.Enabled {
		uh := unmanic.RegisterRoutes(api, clients, &cfg.Providers.Unmanic)
		collectEnder(uh)
		slog.Info("enabled provider", "provider", "unmanic")
	}

	if cfg.Providers.Bazarr.Enabled {
		bazarr.RegisterRoutes(api, clients, &cfg.Providers.Bazarr)
		slog.Info("enabled provider", "provider", "bazarr")
	}

	if cfg.Providers.Proxmox.Enabled {
		pxh := proxmox.RegisterRoutes(api, store, clients, &cfg.Providers.Proxmox)
		collectEnder(pxh)
		slog.Info("enabled provider", "provider", "proxmox")
	}

	if cfg.Providers.Overseerr.Enabled {
		oh := overseerr.RegisterRoutes(api, store, clients, &cfg.Providers.Overseerr, posters)
		collectEnder(oh)
		slog.Info("enabled provider", "provider", "overseerr")
	}

	if cfg.Providers.UptimeKuma.Enabled {
		ukh := uptimekuma.RegisterRoutes(api, store, clients, &cfg.Providers.UptimeKuma)
		collectEnder(ukh)
		slog.Info("enabled provider", "provider", "uptimekuma")
	}

	if cfg.Providers.Gatus.Enabled {
		gah := gatus.RegisterRoutes(api, store, clients, &cfg.Providers.Gatus)
		collectEnder(gah)
		slog.Info("enabled provider", "provider", "gatus")
	}

	if cfg.Providers.Backrest.Enabled {
		bh := backrest.RegisterRoutes(api, store, clients, &cfg.Providers.Backrest)
		collectEnder(bh)
		slog.Info("enabled provider", "provider", "backrest")
	}

	if cfg.Providers.Gitea.Enabled {
		gih := gitea.RegisterRoutes(api, store, clients, &cfg.Providers.Gitea)
		collectEnder(gih)
		slog.Info("enabled provider", "provider", "gitea")
	}

	if cfg.Providers.Komodo.Enabled {
		kh := komodo.RegisterRoutes(api, store, clients, &cfg.Providers.Komodo)
		collectEnder(kh)
		slog.Info("enabled provider", "provider", "komodo")
	}

	if cfg.Providers.TrueNAS.Enabled {
		tnh := truenas.RegisterRoutes(api, store, clients, &cfg.Providers.TrueNAS)
		collectEnder(tnh)
		slog.Info("enabled provider", "provider", "truenas")
	}

	if cfg.Providers.Universal.Enabled {
		if mappings == nil {
			return r, errors.New("the universal route needs a mapping store")
		}
		// Primary stays nil until a ranker passes its gate; Fallback then
		// just runs the heuristic.
		proposer := &universal.Fallback{
			Secondary: universal.Heuristic{},
			OnFallback: func(reason string) {
				metrics.UniversalProposerFallbackTotal.WithLabelValues(reason).Inc()
			},
		}
		uh, err := universalhook.RegisterRoutes(api, state.KeyHashing(raw, state.KeyModeStrict), mappings, clients, &cfg.Providers.Universal, proposer)
		if err != nil {
			return r, err
		}
		collectEnder(uh)
		r.universal = uh
		slog.Info("enabled provider", "provider", "universal")
	}

	return r, nil
}

// sweepMappings runs the mapping store's sweep and refreshes the mapping
// gauge.
func sweepMappings(ctx context.Context, mappings state.MappingStore) {
	res, err := mappings.Sweep(ctx)
	if err != nil {
		slog.Error("universal mapping sweep failed", "error", err)
	} else {
		metrics.UniversalSweptTotal.WithLabelValues("pending").Add(float64(res.Pending))
		metrics.UniversalSweptTotal.WithLabelValues("idle").Add(float64(res.Idle))
		metrics.UniversalSweptTotal.WithLabelValues("samples").Add(float64(res.Samples))
		if res.Pending+res.Idle+res.Samples > 0 {
			slog.Info("universal mapping sweep", "pending", res.Pending, "idle", res.Idle, "samples", res.Samples)
		}
	}
	counts, err := mappings.CountByStatus(ctx)
	if err != nil {
		slog.Warn("universal mapping count failed", "error", err)
		return
	}
	for _, s := range []state.MappingStatus{state.MappingPending, state.MappingConfirmed, state.MappingRejected} {
		metrics.UniversalMappings.WithLabelValues(string(s)).Set(float64(counts[s]))
	}
}
