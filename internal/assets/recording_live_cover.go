package assets

import "time"

// Live covers can exist before a package. Their bytes remain private and immutable.
type RecordingLiveCover struct {
	ID            string    `json:"uploadId"`
	Scope         string    `json:"-"`
	RecordingID   string    `json:"-"`
	Kind          string    `json:"kind"`
	State         string    `json:"state"`
	MIME          string    `json:"-"`
	Digest        string    `json:"-"`
	OutputDigest  string    `json:"-"`
	ClaimID       string    `json:"-"`
	OutputAttempt string    `json:"-"`
	CreatedAt     time.Time `json:"-"`
}

func (c RecordingLiveCover) InputKey() string {
	return "recordings/covers/" + c.Scope + "/" + c.ID + "/input"
}
func (c RecordingLiveCover) Key() string {
	return "recordings/covers/" + c.Scope + "/" + c.OutputAttempt + "/custom.jpg"
}
