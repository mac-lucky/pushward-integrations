package komodo

import "encoding/json"

// komodoPayload is Komodo's Custom-alerter POST body (serialized from its Rust
// Alert type). _id is present only for persisted alerts and, when present, is a
// Mongo extended-JSON object ({"$oid": "..."}); it is decoded tolerantly as raw
// JSON because the activity slug is keyed on the alert condition, not on _id.
type komodoPayload struct {
	ID         json.RawMessage `json:"_id"`
	TS         int64           `json:"ts"` // unix milliseconds
	Resolved   bool            `json:"resolved"`
	Level      string          `json:"level"` // OK | WARNING | CRITICAL
	Target     komodoTarget    `json:"target"`
	Data       komodoData      `json:"data"`
	ResolvedTS *int64          `json:"resolved_ts"`
}

type komodoTarget struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// komodoData is the double-nested alert payload: an outer discriminator (type)
// wrapping the variant fields (data). Only the fields this bridge renders are
// modeled; unknown variant fields are ignored by encoding/json.
type komodoData struct {
	Type string        `json:"type"`
	Data komodoDataDoc `json:"data"`
}

type komodoDataDoc struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	ServerName string          `json:"server_name"`
	Region     *string         `json:"region"`
	Percentage *float64        `json:"percentage"`
	UsedGB     *float64        `json:"used_gb"`
	TotalGB    *float64        `json:"total_gb"`
	Path       string          `json:"path"`
	From       string          `json:"from"`
	To         string          `json:"to"`
	Image      string          `json:"image"`
	Message    string          `json:"message"`
	Err        json.RawMessage `json:"err"`
}

// resolvableTypes are the alert conditions that flip between active and resolved
// (Komodo re-sends the same condition with resolved=true when it clears). These
// drive a Live Activity with a two-phase end; every other data.type is a
// one-shot notification.
var resolvableTypes = map[string]bool{
	"ServerUnreachable":     true,
	"ServerCpu":             true,
	"ServerMem":             true,
	"ServerDisk":            true,
	"ServerVersionMismatch": true,
	"SwarmUnhealthy":        true,
}
