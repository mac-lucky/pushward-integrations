package uptimekuma

type webhookPayload struct {
	Monitor   monitorInfo   `json:"monitor"`
	Heartbeat heartbeatInfo `json:"heartbeat"`
	Msg       string        `json:"msg"`
}

type monitorInfo struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
	Type string `json:"type"` // "http", "tcp", "ping", "dns", "docker"
}

type heartbeatInfo struct {
	Status    int    `json:"status"`    // 0=DOWN, 1=UP, 2=PENDING, 3=MAINTENANCE
	Time      string `json:"time"`
	Msg       string `json:"msg"`       // Error message when down
	Ping      *int   `json:"ping"`      // Response time ms (null when down)
	Duration  int    `json:"duration"`  // Seconds in current state
	Important bool   `json:"important"` // True on state transitions
}
