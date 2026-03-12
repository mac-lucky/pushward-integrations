package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Sanitize path: /grafana/webhook -> grafana_webhook
		sanitized := strings.ReplaceAll(strings.Trim(r.URL.Path, "/"), "/", "_")
		if sanitized == "" {
			sanitized = "root"
		}
		filename := fmt.Sprintf("testdata/%s_%d.json", sanitized, time.Now().UnixMilli())
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(filename, body, 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("captured %s %s -> %s (%d bytes)", r.Method, r.URL.Path, filename, len(body))
		w.WriteHeader(http.StatusOK)
	})

	log.Println("capture-server listening on :9999")
	log.Fatal(http.ListenAndServe(":9999", nil))
}
