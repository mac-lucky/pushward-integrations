package humautil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegisterDelete(t *testing.T) {
	mux, api := NewTestAPI()

	var gotID, gotKind string
	RegisterDelete(api, "/thing/{id}", "delete-thing",
		"Delete a thing", "Deletes a thing by id.",
		[]string{"Test"},
		func(_ context.Context, input *struct {
			ID   string `path:"id"`
			Kind string `query:"kind"`
		},
		) (*WebhookResponse, error) {
			gotID = input.ID
			gotKind = input.Kind
			return NewOK(), nil
		})

	t.Run("extracts path and query params", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/thing/abc123?kind=alias", nil)
		req.Header.Set("Authorization", "Bearer hlk_test")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
		}
		if gotID != "abc123" {
			t.Errorf("path id = %q, want abc123", gotID)
		}
		if gotKind != "alias" {
			t.Errorf("query kind = %q, want alias", gotKind)
		}
	})

	t.Run("requires a valid key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/thing/abc123", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 without a key, got %d", w.Code)
		}
	})
}
