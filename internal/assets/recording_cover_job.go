package assets

import (
	"fmt"
	"time"
)

type RecordingCover struct {
	ID               string    `json:"uploadId"`
	OperationKey     string    `json:"-"`
	PackageID        string    `json:"-"`
	RecordingID      string    `json:"-"`
	Kind             string    `json:"kind"`
	State            string    `json:"state"`
	MIME             string    `json:"-"`
	Digest           string    `json:"-"`
	ClaimID          string    `json:"-"`
	InheritedCoverID string    `json:"-"`
	OutputAttempt    string    `json:"-"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

func (c RecordingCover) InputKey() string {
	return "recordings/covers/" + c.RecordingID + "/" + c.ID + "/input"
}
func (c RecordingCover) Key(index int) string {
	name := "custom.jpg"
	if c.Kind == "auto" || c.Kind == "live-auto" {
		name = fmt.Sprintf("auto-%d.jpg", index)
	}
	return "recordings/covers/" + c.RecordingID + "/" + c.OutputAttempt + "/" + name
}
