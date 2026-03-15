package proxmox

type webhookPayload struct {
	Type     string `json:"type"`     // "vzdump", "replication", "fencing", "package-updates", "system"
	Title    string `json:"title"`    // Short summary
	Message  string `json:"message"`  // Full description
	Severity string `json:"severity"` // "info", "notice", "warning", "error"
	Hostname string `json:"hostname"` // Node name
}
