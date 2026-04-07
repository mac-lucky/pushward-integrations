package apprise

// Payload is the JSON body sent by Apprise's json:// / jsons:// notification plugin.
type Payload struct {
	Version string `json:"version"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Type    string `json:"type"`
}
