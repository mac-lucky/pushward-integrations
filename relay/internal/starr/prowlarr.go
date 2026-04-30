package starr

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

var (
	reTV   = regexp.MustCompile(`(?i)^(.+?)\.S\d{2}`)
	reYear = regexp.MustCompile(`^(.+?)\.(?:19|20)\d{2}\.`)
)

// releaseBaseTitle extracts the media name from a scene release title.
// TV: "Show.Name.S01E02.1080p..." → "Show.Name"
// Movie: "Movie.Name.2024.1080p..." → "Movie.Name"
// Returns empty string if the title cannot be parsed.
func releaseBaseTitle(title string) string {
	if m := reTV.FindStringSubmatch(title); m != nil {
		return m[1]
	}
	if m := reYear.FindStringSubmatch(title); m != nil {
		return m[1]
	}
	return ""
}

func (h *Handler) handleProwlarrWebhook(ctx context.Context, raw []byte) error {
	envelope, err := decodeEnvelope(raw)
	if err != nil {
		return err
	}

	ctx = metrics.WithProvider(ctx, "starr")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))

	var apiErr error
	switch envelope.EventType {
	case "Test":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "prowlarr"); err != nil {
			log.Error("test notification failed", "provider", "prowlarr", "error", err)
		}
	case "Grab":
		p, err := unmarshalPayload[ProwlarrGrabPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleProwlarrGrab(ctx, userKey, log, p)
	case "Health":
		p, err := unmarshalPayload[HealthPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleHealth(ctx, userKey, log, "prowlarr", p)
	case "HealthRestored":
		p, err := unmarshalPayload[HealthRestoredPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleHealthRestored(ctx, userKey, log, "prowlarr", p)
	case "ApplicationUpdate":
		p, err := unmarshalPayload[ApplicationUpdatePayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleApplicationUpdate(ctx, userKey, log, "prowlarr", p)
	default:
		slog.Debug("ignored event", "event_type", envelope.EventType)
	}

	if apiErr != nil {
		return upstreamHumaError(apiErr)
	}
	return nil
}

func (h *Handler) handleProwlarrGrab(ctx context.Context, userKey string, log *slog.Logger, p *ProwlarrGrabPayload) error {
	body := "Grabbed · " + p.Release.Indexer
	if p.Source != "" {
		body += " → " + p.Source
	}

	meta := map[string]string{"indexer": p.Release.Indexer}
	if p.InstanceName != "" {
		meta["instance_name"] = p.InstanceName
	}
	if p.Release.Size != nil && *p.Release.Size > 0 {
		meta["size"] = text.FormatBytes(*p.Release.Size)
	}
	if len(p.Release.Categories) > 0 {
		meta["categories"] = strings.Join(p.Release.Categories, ", ")
	}

	threadID := "prowlarr"
	if base := releaseBaseTitle(p.Release.ReleaseTitle); base != "" {
		threadID = text.Slug("prowlarr-", base)
		meta["media_title"] = strings.ReplaceAll(base, ".", " ")
	}

	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title:      "Prowlarr",
		Subtitle:   text.Truncate(p.Release.ReleaseTitle, 80),
		Body:       body,
		ThreadID:   threadID,
		CollapseID: text.SlugHash("prowlarr-grab", p.Release.ReleaseTitle, 6),
		Level:      pushward.LevelActive,
		Source:     "prowlarr",
		Push:       true,
		Metadata:   meta,
	})
}
