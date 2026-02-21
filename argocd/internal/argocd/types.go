package argocd

// WebhookPayload is the JSON body sent by argocd-notifications webhook templates.
type WebhookPayload struct {
	App            string `json:"app"`
	Project        string `json:"project"`
	Event          string `json:"event"`
	SyncStatus     string `json:"sync_status"`
	HealthStatus   string `json:"health_status"`
	OperationPhase string `json:"operation_phase"`
	Revision       string `json:"revision"`
	Message        string `json:"message"`
	RepoURL        string `json:"repo_url"`
}
