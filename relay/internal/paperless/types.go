package paperless

type webhookPayload struct {
	Event         string `json:"event"`          // "added", "updated", "consumption_started"
	DocID         *int   `json:"doc_id"`         // nil for consumption_started
	Title         string `json:"title"`
	Correspondent string `json:"correspondent"`
	DocumentType  string `json:"document_type"`
	DocURL        string `json:"doc_url"`
	Filename      string `json:"filename"`
	Tags          string `json:"tags"`
}
