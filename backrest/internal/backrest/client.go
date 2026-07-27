package backrest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Backrest speaks ConnectRPC. Unary calls over that protocol are an ordinary
// JSON POST - no envelope, no generated stubs, no dependency - so GetOperations
// is hand-rolled. Only the streaming half (GetLogs) needs real framing.
const (
	pathGetOperations = "/v1.Backrest/GetOperations"
	pathGetLogs       = "/v1.Backrest/GetLogs"

	contentTypeUnary  = "application/json"
	contentTypeStream = "application/connect+json"
	protocolVersion   = "1"
)

// maxLogBytes caps how much of a task log is read into memory. Only the tail is
// ever rendered, but a repo check on a large repository can emit megabytes and
// the whole stream has to be walked to reach its end.
const maxLogBytes = 512 * 1024

// maxFrameBytes bounds a single stream frame. The length prefix is
// attacker-influenced in the sense that a wrong endpoint (or a proxy error
// page) can decode as an enormous frame, and allocating on that would be the
// bug rather than the malformed input.
const maxFrameBytes = 8 * 1024 * 1024

// endStreamFlag marks the final frame of a Connect stream. Its payload is
// stream metadata and any terminal error, not response data.
const endStreamFlag = 0b10

type Client struct {
	httpClient *http.Client
	baseURL    string
	username   string
	password   string
	token      string
}

// Options configure how the client authenticates. Backrest's middleware accepts
// HTTP Basic or a bearer JWT, and passes every request through untouched when
// auth is disabled - so all three fields empty is a valid configuration, not a
// missing one.
type Options struct {
	Username string
	Password string
	Token    string
	Timeout  time.Duration
}

func NewClient(baseURL string, opts Options) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Client{
		httpClient: &http.Client{Timeout: timeout},
		baseURL:    strings.TrimRight(baseURL, "/"),
		username:   opts.Username,
		password:   opts.Password,
		token:      opts.Token,
	}
}

func (c *Client) authorize(req *http.Request) {
	switch {
	case c.token != "":
		req.Header.Set("Authorization", "Bearer "+c.token)
	case c.username != "" || c.password != "":
		req.SetBasicAuth(c.username, c.password)
	}
}

// GetOperations returns the most recent lastN operations, newest last (the
// server orders by id). Backrest runs its task queue serially, so the window
// only has to be wide enough to still contain the running operation after a
// burst of hook and stats rows lands on top of it.
func (c *Client) GetOperations(ctx context.Context, lastN int64) ([]Operation, error) {
	// The empty selector object is required, not decorative: the server maps a
	// nil selector to an "empty selector" error and only a present one to a
	// match-all query. Omitting the key entirely is a 500.
	//
	// lastN is an int64 on the wire, and proto-JSON wants 64-bit integers
	// quoted. The server accepts a bare number too, but sending what it emits
	// keeps one convention in play.
	body := fmt.Sprintf(`{"selector":{},"lastN":"%d"}`, lastN)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+pathGetOperations, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentTypeUnary)
	req.Header.Set("Connect-Protocol-Version", protocolVersion)
	c.authorize(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get operations: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFrameBytes))
	if err != nil {
		return nil, fmt.Errorf("get operations: reading body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get operations: %w", connectError(resp.StatusCode, raw))
	}

	var list OperationList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("get operations: decoding body: %w", err)
	}
	return list.Operations, nil
}

// GetLogs returns the text of a task or command log. Backrest streams it, so
// this walks the Connect envelopes and concatenates the payloads; the result is
// capped at maxLogBytes because only the tail is ever displayed.
func (c *Client) GetLogs(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", errors.New("get logs: empty ref")
	}

	payload, err := json.Marshal(struct {
		Ref string `json:"ref"`
	}{Ref: ref})
	if err != nil {
		return "", err
	}

	frame, err := envelope(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+pathGetLogs, bytes.NewReader(frame))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentTypeStream)
	req.Header.Set("Connect-Protocol-Version", protocolVersion)
	c.authorize(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("get logs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return "", fmt.Errorf("get logs: %w", connectError(resp.StatusCode, raw))
	}
	return readLogStream(resp.Body)
}

// envelope wraps a payload in a Connect stream frame: one flags byte, then a
// big-endian uint32 length, then the payload. Streaming requests are framed the
// same way responses are.
func envelope(payload []byte) ([]byte, error) {
	// The length field is 32 bits. Nothing this client sends comes close, but
	// the bound is what makes the conversion below safe rather than assumed.
	if len(payload) > maxFrameBytes {
		return nil, fmt.Errorf("request payload of %d bytes exceeds the frame limit", len(payload))
	}
	buf := make([]byte, 5+len(payload))
	buf[0] = 0
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload))) // #nosec G115 -- bounded by maxFrameBytes above
	copy(buf[5:], payload)
	return buf, nil
}

// readLogStream walks the response frames. Data frames carry a types.BytesValue
// whose base64 value is a chunk of the log; the end-of-stream frame carries any
// terminal error, which is the only place a streaming RPC can report one - the
// HTTP status is 200 either way.
func readLogStream(r io.Reader) (string, error) {
	var out strings.Builder
	var header [5]byte

	for out.Len() < maxLogBytes {
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				// A stream that stops without its end frame has still delivered
				// whatever arrived; the caller gets the partial log rather than
				// nothing.
				return out.String(), nil
			}
			return out.String(), fmt.Errorf("get logs: reading frame header: %w", err)
		}

		size := binary.BigEndian.Uint32(header[1:5])
		if size > maxFrameBytes {
			return out.String(), fmt.Errorf("get logs: frame of %d bytes exceeds limit", size)
		}

		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return out.String(), fmt.Errorf("get logs: reading frame payload: %w", err)
		}

		if header[0]&endStreamFlag != 0 {
			return out.String(), endStreamError(payload)
		}

		var frame struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(payload, &frame); err != nil {
			return out.String(), fmt.Errorf("get logs: decoding frame: %w", err)
		}
		// bytes fields are base64 in proto-JSON.
		chunk, err := base64.StdEncoding.DecodeString(frame.Value)
		if err != nil {
			return out.String(), fmt.Errorf("get logs: decoding chunk: %w", err)
		}
		out.Write(chunk)
	}
	return out.String(), nil
}

// endStreamError reports the terminal error of a stream, or nil when the stream
// ended cleanly.
func endStreamError(payload []byte) error {
	var end struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &end); err != nil {
		return fmt.Errorf("get logs: decoding end-of-stream frame: %w", err)
	}
	if end.Error == nil {
		return nil
	}
	return fmt.Errorf("get logs: %s: %s", end.Error.Code, end.Error.Message)
}

// connectError turns a failed unary call into an error. Connect reports the
// failure as a JSON body alongside the HTTP status; when that body is not JSON
// (a proxy error page, or Backrest's own plain-text auth rejection) the status
// and a bounded excerpt are all there is to go on.
func connectError(status int, body []byte) error {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &e); err == nil && e.Code != "" {
		return fmt.Errorf("http %d: %s: %s", status, e.Code, e.Message)
	}
	excerpt := strings.TrimSpace(string(body))
	if len(excerpt) > 200 {
		excerpt = excerpt[:200]
	}
	if excerpt == "" {
		return fmt.Errorf("http %d", status)
	}
	return fmt.Errorf("http %d: %s", status, excerpt)
}
