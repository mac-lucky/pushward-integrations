package paperless

type paperlessPayload struct {
	Event         string `json:"event"`
	DocID         *int   `json:"doc_id"`
	Title         string `json:"title"`
	Correspondent string `json:"correspondent"`
	DocumentType  string `json:"document_type"`
	DocURL        string `json:"doc_url"`
	Filename      string `json:"filename"`
	Tags          string `json:"tags"`
}
