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
}

const cacheTTL = 1 * time.Hour

// NewClient creates a new Grafana API client.
func NewClient(baseURL, apiToken string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    baseURL,
		apiToken:   apiToken,
		cache:      make(map[string]*RuleQuery),
	}
}

// ruleUIDPattern extracts the rule UID from a generatorURL like
// "https://grafana.example.com/alerting/<uid>/edit"
var ruleUIDPattern = regexp.MustCompile(`/alerting/([^/]+)/(?:edit|view)`)

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
	if len(c.cache) > 500 {
		for uid, cached := range c.cache {
			if time.Since(cached.FetchedAt) > 2*cacheTTL {
				delete(c.cache, uid)
			}
		}
	}
	c.mu.Unlock()

	return rq, nil
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
