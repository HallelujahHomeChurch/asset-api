package assets

import "time"

// Shared limits for HLS package and browser source upload capabilities.
const (
	RecordingUploadTTL  = 24 * time.Hour
	RecordingPartURLTTL = 15 * time.Minute
)
