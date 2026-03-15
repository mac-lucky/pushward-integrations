package unmanic

type apprisePayload struct {
	Version string `json:"version"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Type    string `json:"type"` // "success", "failure", "info"
}
