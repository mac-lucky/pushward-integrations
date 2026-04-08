package jellyfin

// jellyfinPayload is the JSON body sent by the Jellyfin webhook plugin.
type jellyfinPayload struct {
	NotificationType      string `json:"NotificationType"`
	ServerName            string `json:"ServerName"`
	ServerURL             string `json:"ServerUrl"`
	ItemID                string `json:"ItemId"`
	ItemType              string `json:"ItemType"`
	Name                  string `json:"Name"`
	SeriesName            string `json:"SeriesName"`
	SeasonNumber          int    `json:"SeasonNumber"`
	EpisodeNumber         int    `json:"EpisodeNumber"`
	ProductionYear        int    `json:"ProductionYear"`
	RunTimeTicks          int64  `json:"RunTimeTicks"`
	UserName              string `json:"NotificationUsername"`
	DeviceName            string `json:"DeviceName"`
	ClientName            string `json:"ClientName"`
	PlaybackPositionTicks int64  `json:"PlaybackPositionTicks"`
	IsPaused              bool   `json:"IsPaused"`
	PlayedToCompletion    bool   `json:"PlayedToCompletion"`
	// Provider IDs
	ProviderTmdb string `json:"Provider_tmdb"`
	ProviderTvdb string `json:"Provider_tvdb"`
	// Task fields
	TaskName string `json:"TaskName"`
	TaskID     string `json:"TaskId"`
	TaskResult string `json:"TaskResult"`
	// Auth fields
	RemoteEndPoint string `json:"RemoteEndPoint"`
}
