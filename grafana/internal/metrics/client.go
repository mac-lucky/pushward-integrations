package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// Client queries Prometheus or VictoriaMetrics for time-series data.
type Client struct {
	httpClient *http.Client
	baseURL    string
	username   string
	password   string
	bearer     string
}

// NewClient creates a new metrics client.
func NewClient(baseURL string, opts ...Option) *Client {
	c := &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    baseURL,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Option configures the metrics client.
type Option func(*Client)

// WithBasicAuth sets basic authentication credentials.
func WithBasicAuth(username, password string) Option {
	return func(c *Client) {
		c.username = username
		c.password = password
	}
}

// WithBearerToken sets a bearer token for authentication.
func WithBearerToken(token string) Option {
	return func(c *Client) { c.bearer = token }
}

// QueryRange fetches time-series data for the given PromQL expression.
func (c *Client) QueryRange(ctx context.Context, expr string, from, to time.Time, step time.Duration) ([]pushward.HistoryPoint, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing base URL: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/query_range"
	q := u.Query()
	q.Set("query", expr)
	q.Set("start", strconv.FormatInt(from.Unix(), 10))
	q.Set("end", strconv.FormatInt(to.Unix(), 10))
	q.Set("step", strconv.Itoa(max(1, int(step.Seconds()))))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying metrics: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics query returned %d", resp.StatusCode)
	}

	var result queryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	if result.Status != "success" {
		return nil, fmt.Errorf("query failed: %s: %s", result.ErrorType, result.Error)
	}

	if len(result.Data.Result) == 0 {
		return nil, nil
	}

	return parseValues(result.Data.Result[0].Values), nil
}

// QueryInstant fetches a single data point for the given PromQL expression at the given time.
func (c *Client) QueryInstant(ctx context.Context, expr string, ts time.Time) (*pushward.HistoryPoint, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing base URL: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/query"
	q := u.Query()
	q.Set("query", expr)
	q.Set("time", strconv.FormatInt(ts.Unix(), 10))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying metrics: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics query returned %d", resp.StatusCode)
	}

	var result instantQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	if result.Status != "success" {
		return nil, fmt.Errorf("query failed: %s: %s", result.ErrorType, result.Error)
	}

	if len(result.Data.Result) == 0 {
		return nil, nil
	}

	return parseInstantValue(result.Data.Result[0].Value)
}

// instantQueryResponse is the Prometheus/VictoriaMetrics /api/v1/query response.
type instantQueryResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string         `json:"resultType"`
		Result     []instantResult `json:"result"`
	} `json:"data"`
}

type instantResult struct {
	Metric map[string]string  `json:"metric"`
	Value  []json.RawMessage  `json:"value"` // [timestamp, "value"]
}

func parseInstantValue(pair []json.RawMessage) (*pushward.HistoryPoint, error) {
	if len(pair) != 2 {
		return nil, fmt.Errorf("unexpected value format: %d elements", len(pair))
	}

	var ts float64
	if err := json.Unmarshal(pair[0], &ts); err != nil {
		return nil, fmt.Errorf("parsing timestamp: %w", err)
	}

	var valStr string
	if err := json.Unmarshal(pair[1], &valStr); err != nil {
		return nil, fmt.Errorf("parsing value: %w", err)
	}

	v, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return nil, fmt.Errorf("parsing float value %q: %w", valStr, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil, nil
	}

	return &pushward.HistoryPoint{T: int64(ts), V: v}, nil
}

func (c *Client) setAuth(req *http.Request) {
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	} else if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
}

// queryRangeResponse is the Prometheus/VictoriaMetrics /api/v1/query_range response.
type queryRangeResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string         `json:"resultType"`
		Result     []matrixResult `json:"result"`
	} `json:"data"`
}

type matrixResult struct {
	Metric map[string]string    `json:"metric"`
	Values [][]json.RawMessage  `json:"values"`
}

// parseValues converts the Prometheus [timestamp, "value"] pairs to HistoryPoints.
// Values are JSON strings (e.g. "87.3", "NaN", "+Inf") — NaN and Inf are skipped.
func parseValues(values [][]json.RawMessage) []pushward.HistoryPoint {
	points := make([]pushward.HistoryPoint, 0, len(values))
	for _, pair := range values {
		if len(pair) != 2 {
			continue
		}

		// Timestamp is a JSON number
		var ts float64
		if err := json.Unmarshal(pair[0], &ts); err != nil {
			continue
		}

		// Value is a JSON string (e.g. "87.3", "NaN", "+Inf")
		var valStr string
		if err := json.Unmarshal(pair[1], &valStr); err != nil {
			continue
		}

		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}

		points = append(points, pushward.HistoryPoint{
			T: int64(ts),
			V: v,
		})
	}
	return points
}
