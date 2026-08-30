package selftest

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

type providerTest struct {
	name    string
	content pushward.Content
}

var providers = map[string]providerTest{
	"argocd": {
		name: "ArgoCD Test",
		content: pushward.Content{
			Template:    "steps",
			Progress:    float64(2) / float64(3),
			State:       "Rolling out...",
			Icon:        "arrow.triangle.2.circlepath",
			Subtitle:    "ArgoCD · test-app",
			AccentColor: pushward.ColorBlue,
			CurrentStep: pushward.IntPtr(2),
			TotalSteps:  pushward.IntPtr(3),
		},
	},
	"radarr": {
		name: "Radarr Test",
		content: pushward.Content{
			Template:    "steps",
			Progress:    float64(1) / float64(2),
			State:       "Grabbed",
			Icon:        "arrow.down.circle",
			Subtitle:    "Radarr · Test Movie (2024) · 1080p",
			AccentColor: pushward.ColorBlue,
			CurrentStep: pushward.IntPtr(1),
			TotalSteps:  pushward.IntPtr(2),
		},
	},
	"sonarr": {
		name: "Sonarr Test",
		content: pushward.Content{
			Template:    "steps",
			Progress:    float64(1) / float64(2),
			State:       "Grabbed",
			Icon:        "arrow.down.circle",
			Subtitle:    "Sonarr · Test Show - S01E01 · 1080p",
			AccentColor: pushward.ColorBlue,
			CurrentStep: pushward.IntPtr(1),
			TotalSteps:  pushward.IntPtr(2),
		},
	},
	// Prowlarr is notification-only in the relay, so this card has no lifecycle
	// to mirror - it is the one frame the Test button is meant to produce.
	"prowlarr": {
		name: "Prowlarr Test",
		content: pushward.Content{
			Template:    "generic",
			Progress:    1.0,
			State:       "Grabbed",
			Icon:        "magnifyingglass",
			Subtitle:    "Prowlarr · Test Indexer",
			AccentColor: pushward.ColorBlue,
		},
	},
	"jellyfin": {
		name: "Jellyfin Test",
		content: pushward.Content{
			Template:     "generic",
			Progress:     0.45,
			State:        "Playing on Test Device",
			Icon:         "play.circle.fill",
			Subtitle:     "Jellyfin · Test Movie",
			AccentColor:  pushward.ColorBlue,
			LiveProgress: pushward.BoolPtr(true), // end_date stamped at send time
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
			AccentColor: pushward.ColorBlue,
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
			AccentColor: pushward.ColorOrange,
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
			Subtitle:    "Unmanic · test.mkv",
			AccentColor: pushward.ColorBlue,
		},
	},
	"bazarr": {
		name: "Bazarr Test",
		content: pushward.Content{
			Template:    "generic",
			Progress:    1.0,
			State:       "Downloaded",
			Icon:        "mdi:download",
			Subtitle:    "Bazarr · English · 96% from opensubtitles",
			AccentColor: pushward.ColorGreen,
		},
	},
	"proxmox": {
		name: "Proxmox Test",
		content: pushward.Content{
			Template:    "steps",
			Progress:    float64(1) / float64(2),
			State:       "Backing up...",
			Icon:        "externaldrive.fill.badge.timemachine",
			Subtitle:    "Proxmox · pve1",
			AccentColor: pushward.ColorBlue,
			CurrentStep: pushward.IntPtr(1),
			TotalSteps:  pushward.IntPtr(2),
		},
	},
	"overseerr": {
		name: "Overseerr Test",
		content: pushward.Content{
			Template:    "steps",
			Progress:    float64(1) / float64(4),
			State:       "Requested",
			Icon:        "hourglass",
			Subtitle:    "Overseerr · Test Movie",
			AccentColor: pushward.ColorOrange,
			CurrentStep: pushward.IntPtr(1),
			TotalSteps:  pushward.IntPtr(4),
			// Showcase the segmented weighted/colored steps: the download phase
			// dominates the width, each phase carries its own color.
			StepWeights: []float64{1, 1, 6, 2},
			StepColors:  []string{"indigo", "blue", "orange", "green"},
		},
	},
	"uptimekuma": {
		name: "Uptime Kuma Test",
		content: pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       "Monitor Down",
			Icon:        "exclamationmark.triangle.fill",
			Subtitle:    "Uptime Kuma · Test Monitor",
			AccentColor: pushward.ColorRed,
			Severity:    "critical",
		},
	},
	"gatus": {
		name: "Gatus Test",
		content: pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       "Health Check Failed",
			Icon:        "exclamationmark.triangle.fill",
			Subtitle:    "Gatus · test/api",
			AccentColor: pushward.ColorRed,
			Severity:    "critical",
		},
	},
	"backrest": {
		name: "Backrest Test",
		content: pushward.Content{
			Template:    "generic",
			Progress:    0,
			State:       "Backing up...",
			Icon:        "arrow.triangle.2.circlepath",
			Subtitle:    "Backrest · daily-backup",
			AccentColor: pushward.ColorBlue,
		},
	},
	"komodo": {
		name: "Komodo Test",
		content: pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       "High CPU",
			Icon:        "exclamationmark.triangle.fill",
			Subtitle:    "Komodo · test-server",
			AccentColor: pushward.ColorOrange,
			Severity:    "warning",
		},
	},
}

// SendTest creates a test activity and sends an ONGOING update for the given
// provider, logging any failure and returning it.
//
// Callers route the returned error into whatever their handler already converts
// with humautil.UpstreamError. Answering 200 to a self-test that reached nobody
// is the worst place to swallow a failure: the button exists precisely to tell
// the user whether their configuration works, so a green checkmark on a refused
// key is a wrong answer to the only question being asked.
func SendTest(ctx context.Context, cl *pushward.Client, log *slog.Logger, provider string) error {
	if err := sendTest(ctx, cl, log, provider); err != nil {
		log.Error("test notification failed", "provider", provider, "error", err)
		return err
	}
	return nil
}

func sendTest(ctx context.Context, cl *pushward.Client, log *slog.Logger, provider string) error {
	pt, ok := providers[provider]
	if !ok {
		return fmt.Errorf("unknown provider: %s", provider)
	}

	slug := "relay-test-" + provider

	// dismissal_ttl 0: a diagnostic card should leave the Lock Screen the moment
	// it ends. Set at create rather than on the end frame so it also covers the
	// 120s stale_ttl auto-end, which no PATCH from here ever reaches.
	if err := cl.CreateActivity(ctx, slug, pt.name, 1, 300, 120, pushward.WithDismissalTTL(0)); err != nil {
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
	// For generic live-progress providers, stamp end_date at send time. The
	// providers map is built once at process start, so a static end_date would
	// already be in the past by the time /selftest runs.
	if content.Template == "generic" && content.LiveProgress != nil && *content.LiveProgress {
		content.EndDate = pushward.Int64Ptr(time.Now().Unix() + 3960)
	}

	if err := cl.UpdateActivity(ctx, slug, pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	}); err != nil {
		return fmt.Errorf("update activity: %w", err)
	}

	log.Info("test notification sent", "provider", provider, "slug", slug)
	return nil
}
