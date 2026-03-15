package backrest

type webhookPayload struct {
	Event        string `json:"event"`         // "CONDITION_SNAPSHOT_START", "CONDITION_SNAPSHOT_SUCCESS", etc.
	Plan         string `json:"plan"`          // Plan ID
	Repo         string `json:"repo"`          // Repository name
	SnapshotID   string `json:"snapshot_id"`
	DataAdded    int64  `json:"data_added"`    // Bytes added
	FilesNew     int    `json:"files_new"`
	FilesChanged int    `json:"files_changed"`
	DurationMs   int64  `json:"duration_ms"`
	Error        string `json:"error"`
}
