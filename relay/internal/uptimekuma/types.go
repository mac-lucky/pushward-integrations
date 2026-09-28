package uptimekuma

// uptimekumaPayload is Uptime Kuma's webhook body. Monitor and Heartbeat are
// null in the test notification sent from its notification settings.
type uptimekumaPayload struct {
	Monitor   *monitorInfo   `json:"monitor"`
	Heartbeat *heartbeatInfo `json:"heartbeat"`
	Msg       string         `json:"msg"`
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
