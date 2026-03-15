package backrest

type webhookPayload struct {
	Event        string `json:"event"`
	Plan         string `json:"plan"`
	Repo         string `json:"repo"`
	SnapshotID   string `json:"snapshot_id"`
	DataAdded    int64  `json:"data_added"`
	FilesNew     int    `json:"files_new"`
	FilesChanged int    `json:"files_changed"`
	DurationMs   int64  `json:"duration_ms"`
	Error        string `json:"error"`
}
