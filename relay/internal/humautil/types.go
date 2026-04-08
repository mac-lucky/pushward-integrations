// Package humautil provides shared Huma types and middleware for relay handlers.
package humautil

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

// WebhookResponse is the standard success response for webhook endpoints.
type WebhookResponse struct {
	Body struct {
		Status string `json:"status" example:"ok" doc:"Webhook processing result"`
	}
}

// NewOK returns a WebhookResponse with status "ok".
func NewOK() *WebhookResponse {
	r := &WebhookResponse{}
	r.Body.Status = "ok"
	return r
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
	return mux, api
}
