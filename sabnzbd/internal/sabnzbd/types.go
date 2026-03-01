package sabnzbd

type QueueResponse struct {
	Queue Queue `json:"queue"`
}

type Queue struct {
	Status   string      `json:"status"`
	MBLeft   string      `json:"mbleft"`
	MB       string      `json:"mb"`
	KBPerSec string      `json:"kbpersec"`
	TimeLeft string      `json:"timeleft"`
	Slots    []QueueSlot `json:"slots"`
}

type QueueSlot struct {
	Filename string `json:"filename"`
}

type HistoryResponse struct {
	History History `json:"history"`
}

type History struct {
	Slots []HistorySlot `json:"slots"`
}

type HistorySlot struct {
	Status string `json:"status"`
	Name   string `json:"name"`
}
