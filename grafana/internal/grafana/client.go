package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/syncx"
)

// RuleQuery holds the extracted query details from a Grafana alert rule.
type RuleQuery struct {
	Expr          string
	DatasourceUID string
	RefID         string
	FetchedAt     time.Time
}

// Client queries the Grafana provisioning API to extract alert rule queries.
// Requires an Editor-role service account token.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiToken   string

	mu    sync.RWMutex
	cache map[string]*RuleQuery

	cleanup syncx.Periodic
}

const (
	cacheTTL        = 1 * time.Hour
	cacheMaxEntries = 500
	cleanupInterval = 5 * time.Minute
)

// NewClient creates a new Grafana API client. Starts a background
// goroutine that prunes expired cache entries; call Close to stop it.
func NewClient(baseURL, apiToken string) *Client {
	c := &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    baseURL,
		apiToken:   apiToken,
		cache:      make(map[string]*RuleQuery),
	}
	c.cleanup.Start(context.Background(), cleanupInterval, func(context.Context) {
		c.pruneCache()
	})
	return c
}

// Close stops the background cache-cleanup goroutine. Safe to call
// multiple times.
func (c *Client) Close() {
	c.cleanup.Stop()
}

// pruneCache removes expired entries using a two-pass scan: an RLock'd
// pass collects stale UIDs, then a Lock'd pass deletes them. Each entry
// is re-checked under the write lock to avoid evicting an entry that
// was concurrently refreshed.
func (c *Client) pruneCache() {
	cutoff := time.Now().Add(-cacheTTL)

	c.mu.RLock()
	stale := make([]string, 0)
	for uid, cached := range c.cache {
		if cached.FetchedAt.Before(cutoff) {
			stale = append(stale, uid)
		}
	}
	c.mu.RUnlock()

	if len(stale) == 0 {
		return
	}

	c.mu.Lock()
	for _, uid := range stale {
		if cached, ok := c.cache[uid]; ok && cached.FetchedAt.Before(cutoff) {
			delete(c.cache, uid)
		}
	}
	c.mu.Unlock()
}

// ruleUIDPattern extracts the rule UID from a generatorURL like
// "https://grafana.example.com/alerting/<uid>/edit"           (Grafana <11)
// "https://grafana.example.com/alerting/grafana/<uid>/view"   (Grafana 11+)
var ruleUIDPattern = regexp.MustCompile(`/alerting/(?:grafana/)?([^/]+)/(?:edit|view)`)

// ExtractRuleUID parses the rule UID from a Grafana generatorURL.
// Returns empty string if the URL doesn't match the expected pattern
// or contains path traversal characters.
func ExtractRuleUID(generatorURL string) string {
	m := ruleUIDPattern.FindStringSubmatch(generatorURL)
	if len(m) < 2 {
		return ""
	}
	uid := m[1]
	if strings.ContainsAny(uid, ".%/\\") {
		return ""
	}
	return uid
}

// GetRuleQuery fetches the PromQL expression from a Grafana alert rule.
// Results are cached for 1 hour.
func (c *Client) GetRuleQuery(ctx context.Context, ruleUID string) (*RuleQuery, error) {
	c.mu.RLock()
	if cached, ok := c.cache[ruleUID]; ok && time.Since(cached.FetchedAt) < cacheTTL {
		c.mu.RUnlock()
		return cached, nil
	}
	c.mu.RUnlock()

	reqURL := fmt.Sprintf("%s/api/v1/provisioning/alert-rules/%s", c.baseURL, url.PathEscape(ruleUID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching alert rule: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("alert rule API returned %d (Editor role required)", resp.StatusCode)
	}

	var rule alertRuleResponse
	if err := json.NewDecoder(resp.Body).Decode(&rule); err != nil {
		return nil, fmt.Errorf("decoding alert rule: %w", err)
	}

	rq := extractQuery(rule.Data)
	if rq == nil {
		return nil, fmt.Errorf("no datasource query found in alert rule %s", ruleUID)
	}
	rq.FetchedAt = time.Now()

	c.mu.Lock()
	c.cache[ruleUID] = rq
	if len(c.cache) > cacheMaxEntries {
		// Evict the single oldest entry to keep the map bounded
		// between periodic sweeps.
		var oldestUID string
		var oldestAt time.Time
		for uid, cached := range c.cache {
			if oldestUID == "" || cached.FetchedAt.Before(oldestAt) {
				oldestUID = uid
				oldestAt = cached.FetchedAt
			}
		}
		if oldestUID != "" {
			delete(c.cache, oldestUID)
		}
	}
	c.mu.Unlock()

	return rq, nil
}

// IsAlertFiring checks the Grafana alertmanager API to determine if any
// instances of the given alertname are currently active (firing).
func (c *Client) IsAlertFiring(ctx context.Context, alertname string) (bool, error) {
	filter := fmt.Sprintf(`alertname="%s"`, alertname)
	reqURL := fmt.Sprintf("%s/api/alertmanager/grafana/api/v2/alerts?filter=%s",
		c.baseURL, url.QueryEscape(filter))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("querying alertmanager: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("alertmanager API returned %d", resp.StatusCode)
	}

	var alerts []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&alerts); err != nil {
		return false, fmt.Errorf("decoding alerts: %w", err)
	}

	return len(alerts) > 0, nil
}

// alertRuleResponse is the Grafana provisioning API response for an alert rule.
type alertRuleResponse struct {
	Data []alertQuery `json:"data"`
}

type alertQuery struct {
	RefID         string          `json:"refId"`
	DatasourceUID string          `json:"datasourceUid"`
	Model         json.RawMessage `json:"model"`
}

type queryModel struct {
	Expr string `json:"expr"`
}

// extractQuery finds the first real datasource query (not an expression node)
// and returns its PromQL expression.
func extractQuery(queries []alertQuery) *RuleQuery {
	for _, q := range queries {
		// Skip __expr__ expression nodes (datasourceUid "-100")
		if q.DatasourceUID == "-100" {
			continue
		}

		var m queryModel
		if err := json.Unmarshal(q.Model, &m); err != nil || m.Expr == "" {
			continue
		}

		return &RuleQuery{
			Expr:          m.Expr,
			DatasourceUID: q.DatasourceUID,
			RefID:         q.RefID,
		}
	}
	return nil
}
