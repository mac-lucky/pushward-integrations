package humautil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterPublic(t *testing.T) {
	mux, api := NewTestAPI()
	RegisterPublic(api, "/public/{token}", "post-public", "Public",
		func(_ context.Context, in *struct {
			Token string `path:"token"`
		},
		) (*WebhookResponse, error) {
			return NewOK(), nil
		})
	RegisterWebhook(api, "/private", "post-private", "Private", "Needs a key.", []string{"Test"},
		func(context.Context, *struct{}) (*WebhookResponse, error) { return NewOK(), nil })

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	if w := post("/public/abc", ""); w.Code != http.StatusOK {
		t.Errorf("public route without a key: got %d (%s), want 200", w.Code, w.Body.String())
	}
	if w := post("/private", "{}"); w.Code != http.StatusUnauthorized {
		t.Errorf("webhook route without a key: got %d, want 401", w.Code)
	}
	if p := api.OpenAPI().Paths["/public/{token}"]; p != nil {
		t.Error("a public route must not be in the OpenAPI document")
	}
}

func TestHidden(t *testing.T) {
	_, api := NewTestAPI()
	RegisterWebhook(api, "/hidden", "post-hidden", "Hidden", "", nil,
		func(context.Context, *struct{}) (*WebhookResponse, error) { return NewOK(), nil }, Hidden)
	if p := api.OpenAPI().Paths["/hidden"]; p != nil {
		t.Error("a Hidden webhook must not be in the OpenAPI document")
	}
}
