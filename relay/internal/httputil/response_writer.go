package httputil

import "net/http"

// ResponseCapture wraps http.ResponseWriter to capture the status code.
type ResponseCapture struct {
	http.ResponseWriter
	Status      int
	WroteHeader bool
}

// NewResponseCapture returns a ResponseCapture defaulting to 200 OK.
func NewResponseCapture(w http.ResponseWriter) *ResponseCapture {
	return &ResponseCapture{ResponseWriter: w, Status: http.StatusOK}
}

func (rc *ResponseCapture) WriteHeader(code int) {
	if !rc.WroteHeader {
		rc.Status = code
		rc.WroteHeader = true
	}
	rc.ResponseWriter.WriteHeader(code)
}

func (rc *ResponseCapture) Write(b []byte) (int, error) {
	if !rc.WroteHeader {
		rc.WriteHeader(http.StatusOK)
	}
	return rc.ResponseWriter.Write(b)
}

func (rc *ResponseCapture) Unwrap() http.ResponseWriter {
	return rc.ResponseWriter
}
