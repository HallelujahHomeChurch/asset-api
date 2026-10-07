package assets

import (
	"fmt"
	"math"
	"regexp"
	"strings"
)

type RecordingLiveFragment struct {
	FrameRate float64 `json:"frameRate,omitempty"`
	Start     float64 `json:"start"`
	End       float64 `json:"end"`
	Codecs    string  `json:"codecs"`
}
type RecordingLiveSegment struct {
	Sequence   int                              `json:"sequence"`
	Renditions map[string]RecordingLiveFragment `json:"renditions"`
}

var liveCodecs = regexp.MustCompile(`^avc1\.[a-fA-F0-9]{6},mp4a\.40\.2$`)

func LiveRenditions() [3]RecordingRendition {
	return [3]RecordingRendition{
		{Name: "1080p", Width: 1920, Height: 1080, FrameRate: 30, VideoBitrate: 3000000, AudioBitrate: 128000},
		{Name: "720p", Width: 1280, Height: 720, FrameRate: 30, VideoBitrate: 1500000, AudioBitrate: 128000},
		{Name: "480p", Width: 854, Height: 480, FrameRate: 30, VideoBitrate: 800000, AudioBitrate: 128000},
	}

}

// Missing frame rate belongs only to history produced by the former 30fps validator.
func historicalLiveFrameRate(fragment RecordingLiveFragment) float64 {
	if fragment.FrameRate == 0 {
		return 30
	}
	return fragment.FrameRate
}

// Only a complete, decoded three-rendition batch advances the common waterline.
// Short segments wait for normal seal so a crash cannot manufacture an ENDLIST.
func AppendLiveSegment(history []RecordingLiveSegment, batch map[string]RecordingLiveFragment, normalTail bool) ([]RecordingLiveSegment, error) {
	if len(batch) != 3 {
		return nil, ErrCaptureMissingObjects
	}
	if len(history) >= 1440 {
		return nil, ErrInvalidUpload
	}
	var reference *RecordingLiveFragment
	cloned := make(map[string]RecordingLiveFragment, 3)
	for _, r := range LiveRenditions() {
		value, ok := batch[r.Name]
		if !ok {
			return nil, ErrCaptureMissingObjects
		}
		if value.FrameRate != 30 && value.FrameRate != 30000.0/1001 {
			return nil, ErrInvalidUpload
		}
		tolerance := 1/value.FrameRate + 0.001
		duration := value.End - value.Start
		if math.IsNaN(duration) || math.IsInf(duration, 0) || math.IsNaN(value.Start) || math.IsInf(value.Start, 0) || duration <= 0 || duration > 30+tolerance || !liveCodecs.MatchString(value.Codecs) {
			return nil, ErrInvalidUpload
		}
		if len(history) == 0 {
			if value.Start < -0.1 || value.Start > 0.25 {
				return nil, ErrInvalidUpload
			}
		} else {
			last, ok := history[len(history)-1].Renditions[r.Name]
			if !ok || history[len(history)-1].Sequence != len(history)-1 || math.Abs(last.End-value.Start) > tolerance || last.Codecs != value.Codecs || historicalLiveFrameRate(last) != value.FrameRate || math.Abs(last.End-last.Start-30) > tolerance {
				return nil, ErrInvalidUpload
			}
		}
		firstStart := value.Start
		if len(history) > 0 {
			firstStart = history[0].Renditions[r.Name].Start
		}
		if value.End-firstStart > RecordingMaxDurationSeconds+0.000001 {
			return nil, ErrInvalidUpload
		}
		if reference != nil && (value.FrameRate != reference.FrameRate || math.Abs(value.Start-reference.Start) > tolerance || math.Abs(value.End-reference.End) > tolerance) {
			return nil, ErrInvalidUpload
		}
		if reference == nil {
			copy := value
			reference = &copy
		}
		if !normalTail && duration < 30-tolerance {
			return nil, ErrCaptureMissingObjects
		}
		cloned[r.Name] = value
	}
	return append(history, RecordingLiveSegment{Sequence: len(history), Renditions: cloned}), nil
}

func RecordingLivePlaylists(history []RecordingLiveSegment, ended bool) (map[string][]byte, error) {
	if len(history) == 0 || len(history) > 1440 {
		return nil, ErrInvalidInput
	}
	var verified []RecordingLiveSegment
	for i, segment := range history {
		if segment.Sequence != i {
			return nil, ErrInvalidInput
		}
		var err error
		batch := make(map[string]RecordingLiveFragment, 3)
		for name, fragment := range segment.Renditions {
			fragment.FrameRate = historicalLiveFrameRate(fragment)
			batch[name] = fragment
		}
		verified, err = AppendLiveSegment(verified, batch, ended && i == len(history)-1)
		if err != nil {
			return nil, err
		}
	}
	playlists := make(map[string][]byte, 4)
	var master strings.Builder
	master.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	for _, r := range LiveRenditions() {
		fmt.Fprintf(&master, "#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,FRAME-RATE=%.3f,CODECS=\"%s\"\n%s/index.m3u8\n", r.VideoBitrate+r.AudioBitrate, r.Width, r.Height, historicalLiveFrameRate(history[0].Renditions[r.Name]), history[0].Renditions[r.Name].Codecs, r.Name)
		var playlist strings.Builder
		playlist.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:31\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-MAP:URI=\"init.mp4\"\n")
		for _, segment := range history {
			v := segment.Renditions[r.Name]
			fmt.Fprintf(&playlist, "#EXTINF:%.6f,\nseg-%06d.m4s\n", v.End-v.Start, segment.Sequence)
		}
		if ended {
			playlist.WriteString("#EXT-X-ENDLIST\n")
		}
		if int64(playlist.Len()) > RecordingPlaylistMaxBytes {
			return nil, ErrRecordingPackageTooLarge
		}
		playlists[r.Name+"/index.m3u8"] = []byte(playlist.String())
	}
	playlists["master.m3u8"] = []byte(master.String())
	return playlists, nil
}
