package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// JobResponse is the response from GET /api/job.
type JobResponse struct {
	Job      JobInfo      `json:"job"`
	Progress ProgressInfo `json:"progress"`
	State    string       `json:"state"` // "Operational","Printing","Pausing","Paused","Cancelling","Error","Offline","Finishing"
}

type JobInfo struct {
	File               FileInfo `json:"file"`
	EstimatedPrintTime float64  `json:"estimatedPrintTime"` // seconds
}

type FileInfo struct {
	Name   string `json:"name"`
	Origin string `json:"origin"`
}

type ProgressInfo struct {
	Completion    *float64 `json:"completion"`    // 0-100, nil if not printing
	PrintTime     *int     `json:"printTime"`     // seconds elapsed
	PrintTimeLeft *int     `json:"printTimeLeft"` // seconds remaining
}

// PrinterResponse is the response from GET /api/printer.
type PrinterResponse struct {
	Temperature TemperatureData `json:"temperature"`
}

type TemperatureData struct {
	Tool0 *ToolTemp `json:"tool0"`
	Bed   *ToolTemp `json:"bed"`
}

type ToolTemp struct {
	Actual float64 `json:"actual"`
	Target float64 `json:"target"`
}

// Client is the OctoPrint REST API client.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
}

// NewClient creates a new OctoPrint API client.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    baseURL,
		apiKey:     apiKey,
	}
}

// GetJob returns the current job status.
func (c *Client) GetJob(ctx context.Context) (*JobResponse, error) {
	return doGet[JobResponse](c, ctx, "/api/job")
}

// GetPrinter returns the current printer state including temperatures.
func (c *Client) GetPrinter(ctx context.Context) (*PrinterResponse, error) {
	return doGet[PrinterResponse](c, ctx, "/api/printer")
}

func doGet[T any](c *Client, ctx context.Context, path string) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var result T
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}
