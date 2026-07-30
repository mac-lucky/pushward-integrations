// Package humautil provides shared Huma types and middleware for relay handlers.
package humautil

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// Status is the webhook response's processing result. The set is closed: these
// values are part of the public response contract, so callers can branch on them.
type Status string

const (
	// StatusOK means the webhook was acted on in full.
	StatusOK Status = "ok"

	// StatusIgnored is for a webhook the handler recognised but does not act on
	// at all, such as an event type the provider does not map.
	StatusIgnored Status = "ignored"

	// StatusIgnoredActivity means the notification was delivered but no Live
	// Activity was opened, because the payload lacked the field the activity is
	// keyed on. Answering "ok" in that case is what makes a misconfigured
	// webhook template look like a working one.
	StatusIgnoredActivity Status = "ignored_activity"
)

// WebhookResponse is the standard success response for webhook endpoints.
type WebhookResponse struct {
	Body struct {
		Status Status `json:"status" example:"ok" doc:"Webhook processing result"`
		Detail string `json:"detail,omitempty" doc:"Why the webhook was only partly acted on, when status is not \"ok\""`
	}
}

// NewOK returns a WebhookResponse with status "ok".
func NewOK() *WebhookResponse {
	r := &WebhookResponse{}
	r.Body.Status = StatusOK
	return r
}

// NewIgnored returns a WebhookResponse carrying a non-"ok" status and the
// reason, so a caller inspecting the response body can tell a dropped payload
// from a delivered one.
func NewIgnored(status Status, detail string) *WebhookResponse {
	r := &WebhookResponse{}
	r.Body.Status = status
	r.Body.Detail = detail
	return r
}

// authChallenge is the challenge RFC 9110 section 15.5.2 requires on every 401.
// Bearer is the scheme the relay documents first; the routes that also take HTTP
// Basic deliberately do not advertise it, since a Basic challenge makes browsers
// pop a credential dialog for what is a machine-to-machine endpoint.
var authChallenge = http.Header{"Www-Authenticate": []string{`Bearer realm="pushward-relay"`}}

// UpstreamError maps a pushward API error to the huma error the webhook caller
// should see. The relay forwards the caller's own hlk_ key rather than a
// credential of its own, so an auth failure at the next hop really is about the
// credential this caller supplied: it surfaces unchanged, and the caller
// (Sonarr, Overseerr, etc.) reports "auth failed" instead of a generic 502.
func UpstreamError(err error) error {
	var he *pushward.HTTPError
	if errors.As(err, &he) {
		switch he.StatusCode {
		case http.StatusUnauthorized:
			return huma.ErrorWithHeaders(huma.Error401Unauthorized("upstream rejected integration key"), authChallenge)
		case http.StatusForbidden:
			return huma.Error403Forbidden("upstream denied integration key")
		case http.StatusTooManyRequests:
			return huma.Error429TooManyRequests("upstream rate limited")
		}
	}
	return huma.Error502BadGateway("upstream API error")
}

// webhookSecurity is the shared security requirement for all webhook endpoints.
var webhookSecurity = []map[string][]string{{"bearerAuth": {}}}

// RegisterWebhook registers a POST webhook endpoint with common defaults
// (1 MB body limit, bearer auth, 200 default status).
func RegisterWebhook[I, O any](api huma.API, path, operationID, summary, description string, tags []string, handler func(ctx context.Context, input *I) (*O, error)) {
	huma.Register(api, huma.Operation{
		OperationID:   operationID,
		Method:        http.MethodPost,
		Path:          path,
		Summary:       summary,
		Description:   description,
		Tags:          tags,
		Security:      webhookSecurity,
		MaxBodyBytes:  1 << 20,
		DefaultStatus: http.StatusOK,
	}, handler)
}

// RegisterDelete registers a DELETE webhook endpoint with the same defaults as
// RegisterWebhook (RegisterWebhook is POST-only). Used by providers whose
// upstream clears an alert with a DELETE call (e.g. the OpsGenie protocol
// TrueNAS speaks). The input type declares its own path/query params via huma
// field tags.
func RegisterDelete[I, O any](api huma.API, path, operationID, summary, description string, tags []string, handler func(ctx context.Context, input *I) (*O, error)) {
	huma.Register(api, huma.Operation{
		OperationID:   operationID,
		Method:        http.MethodDelete,
		Path:          path,
		Summary:       summary,
		Description:   description,
		Tags:          tags,
		Security:      webhookSecurity,
		MaxBodyBytes:  1 << 20,
		DefaultStatus: http.StatusOK,
	}, handler)
}

// NewAPI creates a Huma API with standard relay config (additional properties
// allowed, fields optional by default). Returns the mux and API.
func NewAPI(title, version string) (*http.ServeMux, huma.API) {
	mux := http.NewServeMux()
	cfg := huma.DefaultConfig(title, version)
	cfg.AllowAdditionalPropertiesByDefault = true
	cfg.FieldsOptionalByDefault = true
	return mux, humago.New(mux, cfg)
}

// NewTestAPI creates a Huma API suitable for tests, with auth middleware
// pre-registered. Returns the mux as an http.Handler and the API.
func NewTestAPI() (http.Handler, huma.API) {
	mux, api := NewAPI("Test", "1.0.0")
	api.UseMiddleware(AuthMiddleware(api))
	api.UseMiddleware(OverridesMiddleware(api))
	return mux, api
}
