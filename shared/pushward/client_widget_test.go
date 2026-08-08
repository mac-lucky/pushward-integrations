package pushward

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		Slug:    "users",
		Name:    "Users",
		Content: WidgetContent{Template: WidgetTemplateValue, Value: &v, Unit: "users"},
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
		Slug: "x", Name: "X", Content: WidgetContent{Template: WidgetTemplateValue},
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

// The widget content fields added for the 1.6 templates survive a JSON
// round-trip under their server-side names. Re-marshalling the decoded value
// and comparing bytes checks every field without a time.Time DeepEqual.
func TestWidgetContent_TemplateFieldsRoundTrip(t *testing.T) {
	start := time.Date(2026, 8, 8, 6, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 9, 18, 30, 0, 0, time.UTC)
	in := WidgetContent{
		Template:       WidgetTemplateFlow,
		Unit:           "W",
		Points:         []float64{1, 2.5, 3},
		StartDate:      &start,
		EndDate:        &end,
		ExpiredText:    "Out now",
		SubtitleTimer:  &TimerValue{Date: end, Style: TimerStyleRelative},
		StatRows:       []StatRow{{Label: "Next", Value: "12m", Timer: &TimerValue{Date: end}}},
		BatteryDevices: []BatteryDevice{{Name: "Vacuum", Level: Float64Ptr(64), Charging: true, Icon: "robotic.vacuum"}},
		Periods:        []SchedulePeriod{{Start: start, Value: Float64Ptr(0.42), Level: ScheduleLevelLow}},
		Flow: &WidgetFlow{
			Inputs:   []FlowNode{{Name: "Solar", Rate: Float64Ptr(2400), Total: Float64Ptr(12.5)}},
			Output:   &FlowNode{Rate: Float64Ptr(900)},
			Storage:  &FlowNode{Rate: Float64Ptr(-300), Level: Float64Ptr(78)},
			Exchange: &FlowNode{Rate: Float64Ptr(-1200)},
		},
	}

	first, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out WidgetContent
	if err := json.Unmarshal(first, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	second, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("round-trip changed the payload:\n first  = %s\n second = %s", first, second)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(first, &keys); err != nil {
		t.Fatalf("key decode: %v", err)
	}
	for _, k := range []string{"points", "start_date", "end_date", "expired_text", "subtitle_timer", "devices", "periods", "flow"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("missing wire key %q in %s", k, first)
		}
	}
	if d := out.BatteryDevices[0].Level; d == nil || *d != 64 {
		t.Errorf("devices[0].level = %v, want 64", d)
	}
	if l := out.Flow.Storage.Level; l == nil || *l != 78 {
		t.Errorf("flow.storage.level = %v, want 78", l)
	}
	if ts := out.StatRows[0].Timer; ts == nil || !ts.Date.Equal(end) {
		t.Errorf("stat_rows[0].timer = %v, want date %v", ts, end)
	}
}

// The required-but-pointer element fields carry no omitempty on purpose: an
// unset level/value/rate must reach the server as an explicit null and be
// rejected, not disappear from the payload. Adding omitempty to any of these
// would turn a caller's bug into a silently half-built element.
func TestWidgetContent_RequiredElementFieldsMarshalNull(t *testing.T) {
	cases := map[string]any{
		`"level":null`: BatteryDevice{Name: "Vacuum"},
		`"value":null`: SchedulePeriod{Start: time.Now()},
		`"rate":null`:  FlowNode{Name: "Solar"},
	}
	for want, v := range cases {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %T: %v", v, err)
		}
		if !strings.Contains(string(b), want) {
			t.Errorf("%T marshalled as %s, want it to contain %s", v, b, want)
		}
	}
}

// A zero WidgetContent must not emit any of the new keys. UpdateWidget is an
// RFC 7396 merge patch, so a key that leaks in with a zero value overwrites
// whatever the widget already had.
func TestWidgetContent_ZeroValueOmitsTemplateFields(t *testing.T) {
	b, err := json.Marshal(WidgetContent{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(b); got != "{}" {
		t.Errorf("zero WidgetContent = %s, want {}", got)
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
