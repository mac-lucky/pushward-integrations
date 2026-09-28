package rootroute

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/relaytest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universalhook"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// A Lidarr health webhook posted to / with nothing but its User-Agent to go
// on reaches the Lidarr health preset, which only applies under the source
// the root route fills in, and opens its card.
func TestLidarrHealthReachesItsPreset(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	// The test API's mux is a *http.ServeMux behind an http.Handler.
	m, api := humautil.NewTestAPI()
	mux := m.(*http.ServeMux)
	cfg := relaytest.UniversalConfig()
	cfg.Enabled, cfg.Presets = true, true
	h := universalhook.RegisterRoutes(api, state.KeyHashing(state.NewMemoryStore(), state.KeyModeStrict), client.NewPool(srv.URL, nil), cfg, nil)
	t.Cleanup(func() {
		h.Ender().StopAll()
		h.Ender().Wait()
	})
	body, err := os.ReadFile(filepath.Join("..", "universal", "presets", "testdata", "lidarr-health_issue.json"))
	if err != nil {
		t.Fatal(err)
	}
	w := serve(New(mux, Options{}), relaytest.NewRequest("/", body, http.Header{"User-Agent": {"Lidarr/2.5.3.4341"}}))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	got := testutil.WaitForCalls(t, calls, mu, 1, 5*time.Second)
	if got[0].Method != http.MethodPost || got[0].Path != "/activities" || !strings.Contains(string(got[0].Body), `"u-lidarr-`) {
		t.Errorf("first call %s %s %s, want a u-lidarr- card", got[0].Method, got[0].Path, got[0].Body)
	}
}
