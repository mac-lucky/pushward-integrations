package rootroute

import (
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
)

// Document adds POST / to api's OpenAPI document. The Router serves it in
// front of the mux, so the operation is documented only, never registered.
func Document(api huma.API) {
	reg := api.OpenAPI().Components.Schemas
	response := reg.Schema(reflect.TypeOf(humautil.WebhookResponse{}.Body), true, "WebhookResponseBody")
	problem := reg.Schema(reflect.TypeOf(huma.ErrorModel{}), true, "ErrorModel")
	maxSourceLen := 32 // the universal route's limit on ?source=
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "post-root-webhook",
		Method:      http.MethodPost,
		Path:        "/",
		Summary:     "Receive any webhook",
		Description: "Takes a webhook from any service. A payload recognised as one of the relay's providers, " +
			"from its headers (the Radarr, Sonarr and Prowlarr User-Agent; Gitea and Forgejo " +
			"Actions events) or its body, is handled exactly as if it had been posted to that " +
			"provider's route. Anything else goes to the universal route, which maps arbitrary JSON " +
			"onto a notification or Live Activity; with that route off, it gets 404.",
		Tags:     []string{"Root"},
		Security: []map[string][]string{{"bearerAuth": {}}},
		Parameters: []*huma.Param{{
			Name:        "source",
			In:          "query",
			Description: "Names the sending service for the universal route, so its payloads get mappings of their own",
			Schema:      &huma.Schema{Type: huma.TypeString, MaxLength: &maxSourceLen, Pattern: "^[a-z0-9-]*$"},
		}},
		RequestBody: &huma.RequestBody{
			Required: true,
			Content: map[string]*huma.MediaType{
				"application/json": {Schema: &huma.Schema{Type: huma.TypeObject, AdditionalProperties: true}},
			},
		},
		Responses: map[string]*huma.Response{
			"200": {
				Description: "OK",
				Content:     map[string]*huma.MediaType{"application/json": {Schema: response}},
			},
			"default": {
				Description: "Error",
				Content:     map[string]*huma.MediaType{"application/problem+json": {Schema: problem}},
			},
		},
	})
}
