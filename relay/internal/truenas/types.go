package truenas

// createAlert is the OpsGenie "create alert" body TrueNAS POSTs to
// {api_url}/v2/alerts. TrueNAS sends only these three fields: no hostname and no
// level/priority (users pick the alert Level per-service in TrueNAS itself).
type createAlert struct {
	Message     string `json:"message"`
	Alias       string `json:"alias"`
	Description string `json:"description"`
}

// trackedAlert is the state-store record for an open alert. The clear call is a
// DELETE that carries only the alias, so the slug and title are persisted here
// to render the resolve frame without the original body.
type trackedAlert struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
}
