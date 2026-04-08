package changedetection

type changedetectionPayload struct {
	URL           string `json:"url"`
	Title         string `json:"title"`
	Tag           string `json:"tag"`
	DiffURL       string `json:"diff_url"`
	PreviewURL    string `json:"preview_url"`
	TriggeredText string `json:"triggered_text"`
	Timestamp     string `json:"timestamp"`
}
