package rootroute

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/relaytest"
)

// toUniversal are the fixtures POST / hands to the universal route although
// a provider has them: TrueNAS speaks OpsGenie's protocol, which is nobody's
// in particular, and the Gitea route only follows Actions runs.
var toUniversal = map[string]bool{
	"truenas/create.json":     true,
	"truenas/test_alert.json": true,
	"gitea/push_ignored.json": true,
}

// TestDispatchMatchesDedicatedRoutes posts every fixture to / with its
// sender's headers, on a relay with every provider and the universal route,
// and compares the result with posting it to its own route on a relay with
// that provider alone: the same status, response and PushWard calls.
func TestDispatchMatchesDedicatedRoutes(t *testing.T) {
	routes := append(relaytest.Routes(), relaytest.Universal("universal", ""))
	total := 0
	for _, rt := range routes {
		names, err := filepath.Glob(filepath.Join(relaytest.Testdata, rt.Dir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(names)
		for _, name := range names {
			total++
			fixture := rt.Dir + "/" + filepath.Base(name)
			t.Run(fixture, func(t *testing.T) {
				t.Parallel()
				h := relaytest.Relay(t)
				root := humautil.NormalizeJSONContentType(New(h.Mux, Options{}))
				got := h.Run(t, root, "/", rt.Dir, name, relaytest.SenderHeaders)

				if toUniversal[fixture] {
					if got.Pattern != "POST "+Universal || got.Status != http.StatusOK {
						t.Errorf("POST / went to %q with %d, want 200 from POST %s: %s", got.Pattern, got.Status, Universal, got.Response)
					}
					// A shape the route does not know is one plain
					// notification.
					calls := relaytest.Calls(got.Calls)
					if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Path != "/notifications" {
						t.Fatalf("calls = %s, want one notification", dump(calls))
					}
					var n struct {
						Title   string            `json:"title"`
						Body    string            `json:"body"`
						Level   string            `json:"level"`
						Actions []json.RawMessage `json:"actions"`
					}
					if err := json.Unmarshal(calls[0].Body, &n); err != nil {
						t.Fatal(err)
					}
					if n.Title == "" || n.Body == "" || n.Level != "active" || len(n.Actions) != 0 {
						t.Errorf("notification = %s", calls[0].Body)
					}
					return
				}
				if got.Pattern != "POST "+rt.Path {
					t.Fatalf("POST / went to %q, want POST %s", got.Pattern, rt.Path)
				}
				want := relaytest.RunDedicated(t, rt, name)
				if got.Status != want.Status || !sameResponse(got.Response, want.Response) {
					t.Errorf("POST / answered %d %s, POST %s answered %d %s", got.Status, got.Response, rt.Path, want.Status, want.Response)
				}
				g, w := relaytest.NormalizeCalls(got.Calls), relaytest.NormalizeCalls(want.Calls)
				if !reflect.DeepEqual(g, w) {
					t.Errorf("calls differ\nPOST /:\n%s\nPOST %s:\n%s", dump(g), rt.Path, dump(w))
				}
			})
		}
	}
	if total < 90 {
		t.Errorf("only %d fixtures", total)
	}
}

// sameResponse compares two response bodies without their $schema link:
// huma adds one only for a schema some visible operation registered, and the
// universal route is hidden, so alone it answers without one.
func sameResponse(a, b string) bool {
	var ma, mb map[string]any
	if json.Unmarshal([]byte(a), &ma) != nil || json.Unmarshal([]byte(b), &mb) != nil {
		return a == b
	}
	delete(ma, "$schema")
	delete(mb, "$schema")
	return reflect.DeepEqual(ma, mb)
}

func dump(calls []relaytest.Call) string {
	b, _ := json.MarshalIndent(calls, "", "  ")
	return string(b)
}
