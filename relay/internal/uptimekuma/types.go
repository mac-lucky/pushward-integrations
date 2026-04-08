package uptimekuma

type uptimekumaPayload struct {
	Monitor   monitorInfo   `json:"monitor"`
	Heartbeat heartbeatInfo `json:"heartbeat"`
	Msg       string        `json:"msg"`
}

type monitorInfo struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
	Type string `json:"type"`
}

type heartbeatInfo struct {
	Status    int    `json:"status"`
	Time      string `json:"time"`
	Msg       string `json:"msg"`
	Ping      *int   `json:"ping"`
	Duration  int    `json:"duration"`
	Important bool   `json:"important"`
}
