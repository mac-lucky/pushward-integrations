package backrest

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.HandlerFunc, opts Options) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, opts)
}

func TestGetOperationsDecodesWindow(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "operations_list.json"))
	if err != nil {
		t.Fatal(err)
	}

	var gotPath, gotBody, gotProto, gotType string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotProto = r.Header.Get("Connect-Protocol-Version")
		gotType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}, Options{})

	ops, err := c.GetOperations(context.Background(), 50)
	if err != nil {
		t.Fatalf("GetOperations: %v", err)
	}
	if len(ops) != 3 {
		t.Fatalf("got %d operations, want 3", len(ops))
	}
	if gotPath != pathGetOperations {
		t.Errorf("path = %q, want %q", gotPath, pathGetOperations)
	}
	if gotProto != protocolVersion {
		t.Errorf("Connect-Protocol-Version = %q, want %q", gotProto, protocolVersion)
	}
	// A unary Connect call is plain JSON, not the enveloped stream media type.
	if gotType != contentTypeUnary {
		t.Errorf("Content-Type = %q, want %q", gotType, contentTypeUnary)
	}
	// The selector object has to be present: the server turns a nil selector
	// into an "empty selector" 500 and only a present one into a match-all
	// query. lastN is an int64 on the wire, so it goes out quoted like the
	// server emits it.
	if gotBody != `{"selector":{},"lastN":"50"}` {
		t.Errorf("body = %q, want {\"selector\":{},\"lastN\":\"50\"}", gotBody)
	}

	var req struct {
		Selector *map[string]any `json:"selector"`
	}
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("request body is not valid JSON: %v", err)
	}
	if req.Selector == nil {
		t.Error("selector decoded as nil, which the server rejects with a 500")
	}
}

func TestGetOperationsSurfacesStructuredError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"bad credentials"}`))
	}, Options{})

	_, err := c.GetOperations(context.Background(), 10)
	if err == nil {
		t.Fatal("GetOperations succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "unauthenticated") || !strings.Contains(err.Error(), "bad credentials") {
		t.Errorf("error = %v, want the Connect code and message", err)
	}
}

// Backrest rejects an unauthenticated request with plain text, not JSON, so the
// error path cannot assume a structured body.
func TestGetOperationsSurfacesPlainTextError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Unauthorized (No Authorization Header)"))
	}, Options{})

	_, err := c.GetOperations(context.Background(), 10)
	if err == nil {
		t.Fatal("GetOperations succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "No Authorization Header") {
		t.Errorf("error = %v, want the status and the body excerpt", err)
	}
}

func TestBasicAuth(t *testing.T) {
	var user, pass string
	var ok bool
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok = r.BasicAuth()
		_, _ = w.Write([]byte(`{"operations":[]}`))
	}, Options{Username: "someone", Password: "secret"})

	if _, err := c.GetOperations(context.Background(), 1); err != nil {
		t.Fatalf("GetOperations: %v", err)
	}
	if !ok || user != "someone" || pass != "secret" {
		t.Errorf("basic auth = (%q, %q, %v), want (someone, secret, true)", user, pass, ok)
	}
}

func TestBearerAuthWinsOverBasic(t *testing.T) {
	var authHeader string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"operations":[]}`))
	}, Options{Username: "someone", Password: "secret", Token: "jwt-value"})

	if _, err := c.GetOperations(context.Background(), 1); err != nil {
		t.Fatalf("GetOperations: %v", err)
	}
	if authHeader != "Bearer jwt-value" {
		t.Errorf("Authorization = %q, want the bearer token", authHeader)
	}
}

// Backrest passes every request through when auth is disabled, so no
// credentials must mean no header rather than an empty one.
func TestNoCredentialsSendsNoAuthHeader(t *testing.T) {
	var present bool
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Authorization"]
		_, _ = w.Write([]byte(`{"operations":[]}`))
	}, Options{})

	if _, err := c.GetOperations(context.Background(), 1); err != nil {
		t.Fatalf("GetOperations: %v", err)
	}
	if present {
		t.Error("an Authorization header was sent with no credentials configured")
	}
}

// frame builds a Connect stream envelope: flags byte, big-endian length, then
// the payload.
func frame(flags byte, payload []byte) []byte {
	buf := make([]byte, 5+len(payload))
	buf[0] = flags
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload))) // #nosec G115 -- test payloads are a few hundred bytes
	copy(buf[5:], payload)
	return buf
}

// dataFrame wraps log text the way the server does: a types.BytesValue whose
// bytes field is base64 in proto-JSON.
func dataFrame(text string) []byte {
	body, _ := json.Marshal(struct {
		Value string `json:"value"`
	}{Value: base64.StdEncoding.EncodeToString([]byte(text))})
	return frame(0, body)
}

func TestGetLogsReassemblesStream(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "testdata", "prune_output.log"))
	if err != nil {
		t.Fatal(err)
	}

	var gotType, gotRef string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")

		// The request is enveloped too, so unwrap it before reading the ref.
		body, _ := io.ReadAll(r.Body)
		if len(body) < 5 {
			t.Errorf("request body is %d bytes, too short to be enveloped", len(body))
			return
		}
		var req struct {
			Ref string `json:"ref"`
		}
		_ = json.Unmarshal(body[5:], &req)
		gotRef = req.Ref

		w.Header().Set("Content-Type", contentTypeStream)
		// Split across frames: the client must concatenate, not take the first.
		half := len(want) / 2
		_, _ = w.Write(dataFrame(string(want[:half])))
		_, _ = w.Write(dataFrame(string(want[half:])))
		_, _ = w.Write(frame(endStreamFlag, []byte(`{}`)))
	}, Options{})

	got, err := c.GetLogs(context.Background(), "c-abc")
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if got != string(want) {
		t.Errorf("log text did not round-trip through the stream framing")
	}
	if gotType != contentTypeStream {
		t.Errorf("Content-Type = %q, want %q", gotType, contentTypeStream)
	}
	if gotRef != "c-abc" {
		t.Errorf("ref = %q, want c-abc", gotRef)
	}
}

// A streaming RPC answers 200 and reports its failure in the end-of-stream
// frame, so the HTTP status alone can never be trusted.
func TestGetLogsSurfacesEndStreamError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeStream)
		_, _ = w.Write(dataFrame("partial output\n"))
		_, _ = w.Write(frame(endStreamFlag, []byte(`{"error":{"code":"not_found","message":"no such log"}}`)))
	}, Options{})

	got, err := c.GetLogs(context.Background(), "c-missing")
	if err == nil {
		t.Fatal("GetLogs succeeded, want the stream error")
	}
	if !strings.Contains(err.Error(), "not_found") || !strings.Contains(err.Error(), "no such log") {
		t.Errorf("error = %v, want the stream's code and message", err)
	}
	// Whatever did arrive is still returned; the caller can show it.
	if got != "partial output\n" {
		t.Errorf("got %q, want the frames that arrived before the error", got)
	}
}

// A stream cut off before its end frame has still delivered something, and the
// tail of a log is worth more than an error about the missing terminator.
func TestGetLogsReturnsPartialOnTruncatedStream(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeStream)
		_, _ = w.Write(dataFrame("first line\n"))
	}, Options{})

	got, err := c.GetLogs(context.Background(), "c-abc")
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if got != "first line\n" {
		t.Errorf("got %q, want the frame that did arrive", got)
	}
}

func TestGetLogsRejectsOversizedFrame(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeStream)
		var header [5]byte
		binary.BigEndian.PutUint32(header[1:5], maxFrameBytes+1)
		_, _ = w.Write(header[:])
	}, Options{})

	if _, err := c.GetLogs(context.Background(), "c-abc"); err == nil {
		t.Fatal("GetLogs accepted a frame past the size limit")
	}
}

func TestGetLogsRejectsEmptyRef(t *testing.T) {
	c := NewClient("http://example.invalid", Options{})
	if _, err := c.GetLogs(context.Background(), ""); err == nil {
		t.Fatal("GetLogs accepted an empty ref")
	}
}

func TestBaseURLTrailingSlashIsTrimmed(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"operations":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/", Options{})
	if _, err := c.GetOperations(context.Background(), 1); err != nil {
		t.Fatalf("GetOperations: %v", err)
	}
	if path != pathGetOperations {
		t.Errorf("path = %q, want %q (no doubled slash)", path, pathGetOperations)
	}
}

// A long log must yield its END, not its beginning. Stopping at the first
// maxLogBytes returned the start of a big repo check, and the renderer then
// showed its "last" lines from a fraction of the way in.
func TestGetLogsKeepsTheTailOfALongStream(t *testing.T) {
	var sb strings.Builder
	const lines = 40000
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&sb, "log line %d\n", i)
	}
	full := sb.String()
	if len(full) <= maxLogBytes {
		t.Fatalf("fixture is %d bytes, need more than the %d-byte window", len(full), maxLogBytes)
	}

	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeStream)
		// Chunked, so the tail has to be assembled across frames.
		for i := 0; i < len(full); i += 8192 {
			end := min(i+8192, len(full))
			_, _ = w.Write(dataFrame(full[i:end]))
		}
		_, _ = w.Write(frame(endStreamFlag, []byte(`{}`)))
	}, Options{})

	got, err := c.GetLogs(context.Background(), "c-big")
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(got) > maxLogBytes {
		t.Errorf("retained %d bytes, want at most %d", len(got), maxLogBytes)
	}
	wantLast := fmt.Sprintf("log line %d", lines)
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), wantLast) {
		tail := got
		if len(tail) > 60 {
			tail = tail[len(tail)-60:]
		}
		t.Errorf("stream ends with %q, want it to end at %q", tail, wantLast)
	}
	// And it must be a contiguous tail, not a stitched-together sample.
	if !strings.Contains(got, fmt.Sprintf("log line %d\nlog line %d\n", lines-1, lines)) {
		t.Error("the retained tail is not contiguous")
	}
}

// A streaming response is 200 whatever happens, so a proxy error page reaches
// the same code path as a real stream. Its bytes must not be fed to the
// envelope parser.
func TestGetLogsRejectsNonStreamContentType(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	}, Options{})

	if _, err := c.GetLogs(context.Background(), "c-abc"); err == nil {
		t.Fatal("GetLogs parsed an HTML error page as a Connect stream")
	}
}

// The client never negotiates compression, so a compressed frame means gzip
// bytes are about to hit a JSON decoder. Fail with something readable instead.
func TestGetLogsRejectsCompressedFrame(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeStream)
		_, _ = w.Write(frame(compressedFlag, []byte("\x1f\x8b not json")))
	}, Options{})

	_, err := c.GetLogs(context.Background(), "c-abc")
	if err == nil {
		t.Fatal("GetLogs accepted a compressed frame")
	}
	if !strings.Contains(err.Error(), "compress") {
		t.Errorf("error = %v, want it to name compression", err)
	}
}
