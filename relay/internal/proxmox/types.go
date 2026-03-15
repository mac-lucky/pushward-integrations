package proxmox

type webhookPayload struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
	Hostname string `json:"hostname"`
}
