package selftest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

type providerTest struct {
	name    string
	content pushward.Content
}

var providers = map[string]providerTest{
	"grafana": {
		name: "Grafana Test",
		content: pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       "CPU usage above 90%",
			Icon:        "exclamationmark.triangle.fill",
			Subtitle:    "Grafana",
			AccentColor: "#FF9500",
			Severity:    "warning",
		},
	},
	"argocd": {
		name: "ArgoCD Test",
		content: pushward.Content{
			Template:    "pipeline",
			Progress:    float64(2) / float64(3),
			State:       "Rolling out...",
			Icon:        "arrow.triangle.2.circlepath",
			Subtitle:    "ArgoCD \u00b7 test-app",
			AccentColor: "#007AFF",
			CurrentStep: pushward.IntPtr(2),
			TotalSteps:  pushward.IntPtr(3),
		},
	},
	"radarr": {
		name: "Radarr Test",
		content: pushward.Content{
			Template:    "pipeline",
			Progress:    float64(1) / float64(2),
			State:       "Grabbed",
			Icon:        "arrow.down.circle",
			Subtitle:    "Radarr \u00b7 Test Movie (2024) \u00b7 1080p",
			AccentColor: "#007AFF",
			CurrentStep: pushward.IntPtr(1),
			TotalSteps:  pushward.IntPtr(2),
		},
	},
	"sonarr": {
		name: "Sonarr Test",
		content: pushward.Content{
			Template:    "pipeline",
			Progress:    float64(1) / float64(2),
			State:       "Grabbed",
			Icon:        "arrow.down.circle",
			Subtitle:    "Sonarr \u00b7 Test Show - S01E01 \u00b7 1080p",
			AccentColor: "#007AFF",
			CurrentStep: pushward.IntPtr(1),
			TotalSteps:  pushward.IntPtr(2),
		},
	},
	"jellyfin": {
		name: "Jellyfin Test",
		content: pushward.Content{
			Template:    "generic",
			Progress:    0.45,
			State:       "Playing on Test Device",
			Icon:        "play.circle.fill",
			Subtitle:    "Jellyfin \u00b7 Test Movie",
			AccentColor: "#007AFF",
		},
	},
	"paperless": {
		name: "Paperless Test",
		content: pushward.Content{
			Template:    "generic",
			Progress:    0,
			State:       "Processing...",
			Icon:        "arrow.triangle.2.circlepath",
			Subtitle:    "Paperless",
			AccentColor: "#007AFF",
		},
	},
	"changedetection": {
		name: "Changedetection Test",
		content: pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       "Page changed",
			Icon:        "eye.fill",
			Subtitle:    "Changedetection",
			AccentColor: "#FF9500",
			Severity:    "info",
		},
	},
	"unmanic": {
		name: "Unmanic Test",
		content: pushward.Content{
			Template:    "generic",
			Progress:    0,
			State:       "Transcoding...",
			Icon:        "arrow.triangle.2.circlepath",
			Subtitle:    "Unmanic \u00b7 test.mkv",
			AccentColor: "#007AFF",
		},
	},
	"proxmox": {
		name: "Proxmox Test",
		content: pushward.Content{
			Template:    "pipeline",
			Progress:    float64(1) / float64(2),
			State:       "Backing up...",
			Icon:        "externaldrive.fill.badge.timemachine",
			Subtitle:    "Proxmox \u00b7 pve1",
			AccentColor: "#007AFF",
			CurrentStep: pushward.IntPtr(1),
			TotalSteps:  pushward.IntPtr(2),
		},
	},
	"overseerr": {
		name: "Overseerr Test",
		content: pushward.Content{
			Template:    "pipeline",
			Progress:    float64(1) / float64(4),
			State:       "Requested",
			Icon:        "hourglass",
			Subtitle:    "Overseerr \u00b7 Test Movie",
			AccentColor: "#FF9500",
			CurrentStep: pushward.IntPtr(1),
			TotalSteps:  pushward.IntPtr(4),
		},
	},
	"uptimekuma": {
		name: "Uptime Kuma Test",
		content: pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       "Monitor Down",
			Icon:        "exclamationmark.triangle.fill",
			Subtitle:    "Uptime Kuma \u00b7 Test Monitor",
			AccentColor: "#FF3B30",
			Severity:    "error",
		},
	},
	"gatus": {
		name: "Gatus Test",
		content: pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       "Health Check Failed",
			Icon:        "exclamationmark.triangle.fill",
			Subtitle:    "Gatus \u00b7 test/api",
			AccentColor: "#FF3B30",
			Severity:    "error",
		},
	},
	"backrest": {
		name: "Backrest Test",
		content: pushward.Content{
			Template:    "generic",
			Progress:    0,
			State:       "Backing up...",
			Icon:        "arrow.triangle.2.circlepath",
			Subtitle:    "Backrest \u00b7 daily-backup",
			AccentColor: "#007AFF",
		},
	},
}

var validProviders []string

func init() {
	validProviders = make([]string, 0, len(providers))
	for k := range providers {
		validProviders = append(validProviders, k)
	}
	sort.Strings(validProviders)
}

// ValidProviders returns the sorted list of registered provider names.
func ValidProviders() []string {
	return validProviders
}

// SendTest creates a test activity and sends an ONGOING update for the given provider.
func SendTest(ctx context.Context, cl *pushward.Client, provider string) error {
	pt, ok := providers[provider]
	if !ok {
		return fmt.Errorf("unknown provider: %s", provider)
	}

	slug := "relay-test-" + provider

	if err := cl.CreateActivity(ctx, slug, pt.name, 1, 30, 25); err != nil {
		return fmt.Errorf("create activity: %w", err)
	}

	content := pt.content
	// Deep-copy pointer fields to avoid mutating the shared map entries
	if content.CurrentStep != nil {
		content.CurrentStep = pushward.IntPtr(*content.CurrentStep)
	}
	if content.TotalSteps != nil {
		content.TotalSteps = pushward.IntPtr(*content.TotalSteps)
	}
	// For alert-template providers, set FiredAt to now
	if content.Template == "alert" {
		now := time.Now().Unix()
		content.FiredAt = pushward.Int64Ptr(now)
	}

	if err := cl.UpdateActivity(ctx, slug, pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	}); err != nil {
		return fmt.Errorf("update activity: %w", err)
	}

	slog.Info("test notification sent", "provider", provider, "slug", slug)
	return nil
}

// ProviderTestHandler handles POST /test/{provider} requests.
type ProviderTestHandler struct {
	clients *client.Pool
}

// NewProviderTestHandler creates a new provider test handler.
func NewProviderTestHandler(clients *client.Pool) *ProviderTestHandler {
	return &ProviderTestHandler{clients: clients}
}

// ServeHTTP handles the request by extracting the provider from the path and sending a test.
func (h *ProviderTestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")

	if _, ok := providers[provider]; !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{
			"error":           "unknown provider",
			"valid_providers": ValidProviders(),
		})
		return
	}

	ctx := r.Context()
	userKey := auth.KeyFromContext(ctx)
	cl := h.clients.Get(userKey)

	if err := SendTest(ctx, cl, provider); err != nil {
		slog.Error("test notification failed", "provider", provider, "error", err)
		http.Error(w, "failed to send test notification", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
