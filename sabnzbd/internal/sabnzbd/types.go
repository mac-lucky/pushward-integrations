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
	NzoID    string `json:"nzo_id"`
	Status   string `json:"status"`
}

// SABnzbd sets this on the slot for display only; the job's own status stays
// "Queued". Matching the slot's "labels" entry instead would break, as that one
// is translated.
const statusPropagating = "Propagating"

// Propagating counts the slots held back by the propagation delay. The queue's
// top-level status is derived from throughput alone, so a queue held entirely by
// propagation reports "Idle" with jobs still in it; the per-slot status is the
// only thing separating that from an empty queue.
func (q *Queue) Propagating() int {
	n := 0
	for _, s := range q.Slots {
		if s.Status == statusPropagating {
			n++
		}
	}
	return n
}

type HistoryResponse struct {
	History History `json:"history"`
}

type History struct {
	Slots []HistorySlot `json:"slots"`
}

type HistorySlot struct {
	Status       string `json:"status"`
	Name         string `json:"name"`
	Bytes        int64  `json:"bytes"`
	DownloadTime int    `json:"download_time"`
	Completed    int64  `json:"completed"` // Unix timestamp when download completed
}
