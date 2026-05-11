package widgets

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mac-lucky/pushward-integrations/grafana/internal/metrics"
)

const statValuePayload = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"x"},"value":[1700000000,"42"]}]}}`

const statEmptyPayload = `{"status":"success","data":{"resultType":"vector","result":[]}}`

func newPromMux(routes map[string]string) (*metrics.Client, func()) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Query().Get("query")]
		if !ok {
			body = statEmptyPayload
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	return metrics.NewClient(srv.URL), srv.Close
}

func TestNewStatListSource_RejectsBadInputs(t *testing.T) {
	mc, close := newPromMux(map[string]string{"up": statValuePayload})
	defer close()

	cases := []struct {
		name string
		in   StatListRow
		want string
	}{
		{"missing label", StatListRow{Query: "up", ValueTemplate: "{{.Value}}"}, "label is required"},
		{"missing query", StatListRow{Label: "L", ValueTemplate: "{{.Value}}"}, "query is required"},
		{"missing template", StatListRow{Label: "L", Query: "up"}, "value_template is required"},
		{"unparseable template", StatListRow{Label: "L", Query: "up", ValueTemplate: "{{.Value"}, "parsing value_template"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewStatListSource(mc, []StatListRow{c.in})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want containing %q", err, c.want)
			}
		})
	}

	if _, err := NewStatListSource(mc, nil); err == nil {
		t.Error("expected error for empty rows")
	}
	if _, err := NewStatListSource(nil, []StatListRow{{Label: "L", Query: "up", ValueTemplate: "{{.Value}}"}}); err == nil {
		t.Error("expected error for nil client")
	}
}

func TestStatListSource_RendersRowsAndMissing(t *testing.T) {
	mc, close := newPromMux(map[string]string{"users": statValuePayload})
	defer close()

	src, err := NewStatListSource(mc, []StatListRow{
		{Label: "Users", Query: "users", ValueTemplate: `{{printf "%.0f" .Value}}`},
		{Label: "Missing", Query: "does_not_exist", ValueTemplate: `{{.Value}}`, MissingValue: "?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := src.Rows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].Value != "42" {
		t.Errorf("row 0 value = %q, want 42", rows[0].Value)
	}
	if rows[1].Value != "?" {
		t.Errorf("row 1 value = %q, want ? (missing)", rows[1].Value)
	}
}

func TestStatListSource_TrimsWhitespace(t *testing.T) {
	mc, close := newPromMux(map[string]string{"x": statValuePayload})
	defer close()

	src, err := NewStatListSource(mc, []StatListRow{
		{Label: "X", Query: "x", ValueTemplate: "  {{printf \"%.0f\" .Value}}  "},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := src.Rows(context.Background())
	if rows[0].Value != "42" {
		t.Errorf("value = %q, want trimmed 42", rows[0].Value)
	}
}
