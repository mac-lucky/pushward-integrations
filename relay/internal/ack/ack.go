// Package ack makes a relay alert repeat until someone acknowledges it, when
// the webhook URL asks for that with ?ack=1 (or a route opts in by its path),
// and stops the repeats when the alert resolves. A provider calls Send for an
// alert's notification and Cancel at the top of its resolve path.
//
// Nothing is kept between the two: the tag is rebuilt on resolve from the same
// identity the alert was sent with, so a relay restart in between, or a resolve
// landing on another replica, still cancels.
package ack

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overrides"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// cancelTimeout bounds the cancel a resolve waits for before it sends its own
// notification. A var so tests can shorten it.
var cancelTimeout = 5 * time.Second

const tagPrefix = "relay."

// Tag is the receipt tag of the alert identity names: relay.<identity>, or
// relay.<hash of identity> when that would not be a valid tag (1-64 printable
// ASCII characters, no spaces). Every identity the providers pass today is a
// short ASCII slug and keeps its readable form.
func Tag(identity string) string {
	if t := tagPrefix + identity; validTag(t) {
		return t
	}
	return tagPrefix + text.HashHex(identity, 16)
}

func validTag(t string) bool {
	if len(t) > 64 {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] < 0x21 || t[i] > 0x7e {
			return false
		}
	}
	return true
}

// Send sends req. It goes out acknowledged, tagged Tag(identity), when the
// request asked for acknowledge, identity is set and req alerts (push not
// false, level not passive); a req without a collapse id then gets the tag as
// one, so a resend supersedes the receipt instead of adding a second. Pass an
// empty identity for a notification that must not repeat, such as a resolve.
//
// When the server refuses the acknowledge itself (see refusal), req is sent
// once more as it came in: asking for repeats must never cost the alert.
func Send(ctx context.Context, c *pushward.Client, log *slog.Logger, req pushward.SendNotificationRequest, identity string) error {
	if identity == "" || !Wanted(ctx, req) {
		return c.SendNotification(ctx, req)
	}
	a := overrides.FromContext(ctx).Ack()
	tag := Tag(identity)
	acked := req
	acked.Acknowledge = &pushward.NotificationAcknowledge{RepeatSeconds: a.RepeatSeconds, ExpireSeconds: a.ExpireSeconds}
	acked.Tags = []string{tag}
	if acked.CollapseID == "" {
		acked.CollapseID = tag
	}
	err := c.SendNotification(ctx, acked)
	reason := refusal(err)
	if reason == "" {
		return err
	}
	metrics.AckFallbackTotal.WithLabelValues(metrics.ProviderFromContext(ctx), reason).Inc()
	log.Warn("acknowledge refused, sending the alert without it", "tag", tag, "reason", reason, "error", err)
	return c.SendNotification(ctx, req)
}

// Wanted reports whether Send, given an identity, sends req acknowledged:
// the request asked for acknowledge and req alerts (push not false, level not
// passive).
func Wanted(ctx context.Context, req pushward.SendNotificationRequest) bool {
	return overrides.FromContext(ctx).Ack() != nil && req.Level != pushward.LevelPassive && (req.Push == nil || *req.Push)
}

// Reasons Send falls back, the reason label of ack_fallback_total.
const (
	reasonReceiptLimit = "receipt_limit"     // 409: 25 receipts already repeat on the account
	reasonDisabled     = "receipts_disabled" // 422: the server has acknowledged alerts off
	reasonAnswerURL    = "answer_url"        // 422: the server has no public URL to record the tap
	reasonInvalid      = "invalid"           // 400 notification.invalid, the handler's acknowledge rules
	reasonSchema       = "schema"            // 422 without a code, the schema's acknowledge bounds
)

// refusal names why the server refused an acknowledged send, or returns ""
// when err is no such refusal. A 401, 403, 429, 5xx, network error or open
// breaker is not one: the plain send would fail the same way, or the server
// may already have taken the first.
func refusal(err error) string {
	var he *pushward.HTTPError
	if !errors.As(err, &he) {
		return ""
	}
	switch he.StatusCode {
	case http.StatusConflict:
		if he.Code == pushward.ErrCodeNotificationReceiptLimit {
			return reasonReceiptLimit
		}
	case http.StatusBadRequest:
		if he.Code == pushward.ErrCodeNotificationInvalid {
			return reasonInvalid
		}
	case http.StatusUnprocessableEntity:
		switch he.Code {
		case pushward.ErrCodeNotificationReceiptDisabled:
			return reasonDisabled
		case pushward.ErrCodeNotificationAnswerURLUnavailable:
			return reasonAnswerURL
		case "":
			return reasonSchema
		}
	}
	return ""
}

// Cancel stops the repeats of the alert identity names, when the request asked
// for acknowledge. Call it first on a resolve path: before any state lookup,
// since the tag needs none, and before the resolved notification, since the
// repeats carry the alert's collapse id and one landing after "Resolved" would
// replace it. It waits at most cancelTimeout and only logs a failure; the
// resolve carries on either way.
func Cancel(ctx context.Context, c *pushward.Client, log *slog.Logger, identity string) {
	ov := overrides.FromContext(ctx)
	if ov.Ack() == nil || !ov.AllowsNotification() || identity == "" {
		return
	}
	// Accepted risk: the breaker is shared relay-wide, and a cancel it admits
	// as its half-open probe that then times out re-opens it, so the resolved
	// notification sent right after can find it open. It needs the one probe
	// a cooldown allows to land on a cancel that is also slow.
	ctx, cancel := context.WithTimeout(ctx, cancelTimeout)
	defer cancel()
	tag := Tag(identity)
	n, err := c.CancelNotificationReceiptsByTag(ctx, tag)
	if err != nil {
		log.Warn("failed to cancel acknowledged alert", "tag", tag, "error", err)
		return
	}
	log.Info("canceled acknowledged alert", "tag", tag, "canceled", n)
}
