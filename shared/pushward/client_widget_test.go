package pushward

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// CreateWidget sends application/json and the right body.
func TestCreateWidget_Body(t *testing.T) {
	var gotCT string
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	v := 42.0
	err := c.CreateWidget(context.Background(), CreateWidgetRequest{
		Slug:     "users",
		Name:     "Users",
		Template: WidgetTemplateValue,
		Content:  WidgetContent{Value: &v, Unit: "users"},
	})
	if err != nil {
		t.Fatalf("CreateWidget: %v", err)
	}
	if gotPath != "/widgets" {
		t.Errorf("path = %q, want /widgets", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	var req CreateWidgetRequest
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("body decode: %v", err)
	}
	if req.Slug != "users" || req.Name != "Users" {
		t.Errorf("decoded request mismatch: %+v", req)
	}
	if req.Content.Value == nil || *req.Content.Value != 42.0 {
		t.Errorf("value not round-tripped: %+v", req.Content.Value)
	}
}

// UpdateWidget sends the merge-patch+json content type.
func TestUpdateWidget_MergePatchContentType(t *testing.T) {
	var gotCT, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	v := 7.0
	err := c.UpdateWidget(context.Background(), "users", UpdateWidgetRequest{
		Content: &WidgetContent{Value: &v},
	})
	if err != nil {
		t.Fatalf("UpdateWidget: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/widgets/users" {
		t.Errorf("path = %q, want /widgets/users", gotPath)
	}
	if gotCT != "application/merge-patch+json" {
		t.Errorf("Content-Type = %q, want application/merge-patch+json", gotCT)
	}
}

// CreateWidget surfaces widget.limit_exceeded as a typed HTTPError.
func TestCreateWidget_LimitExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"widget.limit_exceeded","title":"limit","status":409}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	err := c.CreateWidget(context.Background(), CreateWidgetRequest{
		Slug: "x", Name: "X", Template: WidgetTemplateValue,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var herr *HTTPError
	if !errAs(err, &herr) {
		t.Fatalf("error type = %T, want *HTTPError", err)
	}
	if herr.Code != ErrCodeWidgetLimitExceeded {
		t.Errorf("Code = %q, want %q", herr.Code, ErrCodeWidgetLimitExceeded)
	}
}

func TestDeleteWidget(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "hlk_test")
	if err := c.DeleteWidget(context.Background(), "abc"); err != nil {
		t.Fatalf("DeleteWidget: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/widgets/abc" {
		t.Errorf("method=%q path=%q, want DELETE /widgets/abc", gotMethod, gotPath)
	}
}

// errAs is a local errors.As wrapper to avoid importing errors in test body.
func errAs(err error, target interface{}) bool {
	if err == nil {
		return false
	}
	if t, ok := target.(**HTTPError); ok {
		if h, ok := err.(*HTTPError); ok {
			*t = h
			return true
		}
	}
	return false
}
