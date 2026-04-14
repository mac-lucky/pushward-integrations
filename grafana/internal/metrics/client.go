package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// LabeledSeries is a single time-series result with its metric labels.
type LabeledSeries struct {
	Labels map[string]string
	Points []pushward.HistoryPoint
}

// LabeledPoint is a single instant-query result with its metric labels.
type LabeledPoint struct {
	Labels map[string]string
	Point  pushward.HistoryPoint
}

// SeriesKey builds a display name from metric labels.
// If preferLabel is set and present, use its value.
// If only one label exists, use its value.
// Otherwise join all as "k=v, k=v" sorted by key.
// The result is truncated to 32 runes to satisfy the server's key length limit.
func SeriesKey(labels map[string]string, preferLabel string) string {
	var key string
	switch {
	case len(labels) == 0:
		return "value"
	case preferLabel != "" && labels[preferLabel] != "":
		key = labels[preferLabel]
	case len(labels) == 1:
		for _, v := range labels {
			key = v
		}
	default:
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+labels[k])
		}
		key = strings.Join(parts, ", ")
	}

	if utf8.RuneCountInString(key) > 32 {
		key = string([]rune(key)[:31]) + "\u2026"
	}
	return key
}

// Client queries Prometheus or VictoriaMetrics for time-series data.
type Client struct {
	httpClient *http.Client
	baseURL    string
	username   string
	password   string
	bearer     string
}

// defaultTimeout is used when WithTimeout is not supplied.
const defaultTimeout = 30 * time.Second

// NewClient creates a new metrics client. Default HTTP timeout is 30s;
// override with WithTimeout.
func NewClient(baseURL string, opts ...Option) *Client {
	c := &Client{
		httpClient: &http.Client{Timeout: defaultTimeout},
		baseURL:    baseURL,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Option configures the metrics client.
type Option func(*Client)

// WithTimeout sets the HTTP client timeout. Non-positive values are ignored.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.httpClient.Timeout = d
		}
	}
}

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

// QueryRange fetches time-series data for the first result series only.
func (c *Client) QueryRange(ctx context.Context, expr string, from, to time.Time, step time.Duration) ([]pushward.HistoryPoint, error) {
	series, err := c.QueryRangeAll(ctx, expr, from, to, step)
	if err != nil || len(series) == 0 {
		return nil, err
	}
	return series[0].Points, nil
}

// QueryInstant fetches a single data point for the first result series only.
func (c *Client) QueryInstant(ctx context.Context, expr string, ts time.Time) (*pushward.HistoryPoint, error) {
	points, err := c.QueryInstantAll(ctx, expr, ts)
	if err != nil || len(points) == 0 {
		return nil, err
	}
	return &points[0].Point, nil
}

// QueryRangeAll fetches time-series data for all result series.
func (c *Client) QueryRangeAll(ctx context.Context, expr string, from, to time.Time, step time.Duration) ([]LabeledSeries, error) {
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

	series := make([]LabeledSeries, 0, len(result.Data.Result))
	for _, r := range result.Data.Result {
		points := parseValues(r.Values)
		if len(points) == 0 {
			continue
		}
		series = append(series, LabeledSeries{
			Labels: filterMetricLabels(r.Metric),
			Points: points,
		})
	}
	return series, nil
}

// QueryInstantAll fetches a single data point for all result series.
func (c *Client) QueryInstantAll(ctx context.Context, expr string, ts time.Time) ([]LabeledPoint, error) {
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

	points := make([]LabeledPoint, 0, len(result.Data.Result))
	for _, r := range result.Data.Result {
		pt, err := parseInstantValue(r.Value)
		if err != nil || pt == nil {
			continue
		}
		points = append(points, LabeledPoint{
			Labels: filterMetricLabels(r.Metric),
			Point:  *pt,
		})
	}
	return points, nil
}

// filterMetricLabels returns labels without the __name__ meta-label.
func filterMetricLabels(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if k == "__name__" {
			continue
		}
		out[k] = v
	}
	return out
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
