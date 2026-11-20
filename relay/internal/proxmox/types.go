package proxmox

type proxmoxPayload struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
	Hostname string `json:"hostname"`
	// JobID is PVE's job-id metadata field: the backup job of a scheduled
	// vzdump run, the replication job. Optional; older body templates omit it.
	JobID string `json:"job_id"`
}
