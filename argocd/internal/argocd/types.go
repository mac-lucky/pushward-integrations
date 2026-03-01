package argocd

// WebhookPayload is the JSON body sent by argocd-notifications webhook templates.
type WebhookPayload struct {
	App      string `json:"app"`
	Event    string `json:"event"`
	Revision string `json:"revision"`
	RepoURL  string `json:"repo_url"`
}
