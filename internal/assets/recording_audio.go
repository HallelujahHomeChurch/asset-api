package assets

const RecordingAudioVersion = "aac-hls-v1"

// RecordingAudio is optional: original video availability never depends on it.
type RecordingAudio struct {
	Version      string `json:"version"`
	State        string `json:"state"`
	PlaylistPath string `json:"playlistPath,omitempty"`
}
