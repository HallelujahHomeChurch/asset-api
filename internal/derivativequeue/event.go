package derivativequeue

import "time"

type Request struct {
	EventID   string
	AssetID   string
	ETag      string
	Attempts  int
	CreatedAt time.Time
}

type Event struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
	EventID string `json:"eventId"`
	AssetID string `json:"assetId"`
	ETag    string `json:"etag"`
}
