package starr

// ProwlarrGrabPayload is sent when Prowlarr grabs a release.
type ProwlarrGrabPayload struct {
	EventType    string          `json:"eventType"`
	InstanceName string          `json:"instanceName"`
	Release      ProwlarrRelease `json:"release"`
	Trigger      string          `json:"trigger"`
	Source       string          `json:"source"`
	Host         string          `json:"host"`
}

// ProwlarrRelease represents a grabbed release from Prowlarr.
type ProwlarrRelease struct {
	ReleaseTitle string   `json:"releaseTitle"`
	Indexer      string   `json:"indexer"`
	Size         *int64   `json:"size"`
	Categories   []string `json:"categories"`
}

// ApplicationUpdatePayload is sent when Prowlarr updates itself.
type ApplicationUpdatePayload struct {
	EventType       string `json:"eventType"`
	Message         string `json:"message"`
	PreviousVersion string `json:"previousVersion"`
	NewVersion      string `json:"newVersion"`
}
