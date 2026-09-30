package assets

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var playlistAttribute = regexp.MustCompile(`([A-Z-]+)=("[^"\r\n]*"|[^,"\r\n]+)`)
var playlistCodecs = regexp.MustCompile(`^avc1\.[0-9a-fA-F]{6},mp4a\.40\.2$`)

// ValidateRecordingMasterCodecs compares declarations with probed AVCC/AAC,
// not the client-provided codec labels.
func ValidateRecordingMasterCodecs(inv RecordingPackageInventory, data []byte, actual map[string]string) error {
	lines, err := recordingPlaylistLines(data)
	if err != nil {
		return err
	}
	if err := validateRecordingMaster(lines, inv.Renditions); err != nil {
		return err
	}
	if len(actual) != len(inv.Renditions) {
		return ErrInvalidUpload
	}
	var pending map[string]string
	for _, line := range lines {
		if line == "" {
			continue
		}
		if pending != nil {
			if !strings.EqualFold(pending["CODECS"], actual[strings.TrimSuffix(line, "/index.m3u8")]) {
				return ErrInvalidUpload
			}
			pending = nil
		} else if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			pending, err = recordingAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateRecordingPlaylists accepts the restricted HHC fMP4 VOD profile, not
// arbitrary HLS. This prevents probe network access through URI/key indirection.
// Actual codec, IDR and packet timelines still require independent media probes.
func ValidateRecordingPlaylists(inv RecordingPackageInventory, files map[string][]byte) error {
	if _, err := ValidateRecordingInventory(inv); err != nil {
		return ErrInvalidUpload
	}
	if len(files) != len(inv.Renditions)+1 {
		return ErrInvalidUpload
	}
	master, err := recordingPlaylistLines(files["master.m3u8"])
	if err != nil {
		return err
	}
	if err := validateRecordingMaster(master, inv.Renditions); err != nil {
		return err
	}
	var reference []float64
	for _, r := range inv.Renditions {
		lines, err := recordingPlaylistLines(files[r.Name+"/index.m3u8"])
		if err != nil {
			return err
		}
		boundaries, err := validateRecordingMedia(lines, r)
		if err != nil {
			return err
		}
		if reference != nil {
			if len(boundaries) != len(reference) {
				return ErrInvalidUpload
			}
			for i, b := range boundaries {
				if math.Abs(b-reference[i]) > 1/r.FrameRate+0.001 {
					return ErrInvalidUpload
				}
			}
		} else {
			reference = boundaries
		}
	}
	return nil
}

func recordingPlaylistLines(data []byte) ([]string, error) {
	if len(data) == 0 || int64(len(data)) > RecordingPlaylistMaxBytes {
		return nil, ErrInvalidUpload
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if lines[0] != "#EXTM3U" {
		return nil, ErrInvalidUpload
	}
	for _, line := range lines {
		if len(line) > 4096 || line != strings.TrimSpace(line) || strings.ContainsAny(line, "\x00\r") {
			return nil, ErrInvalidUpload
		}
	}
	return lines[1:], nil
}

func recordingAttributes(text string) (map[string]string, error) {
	attrs := make(map[string]string)
	for text != "" {
		match := playlistAttribute.FindStringSubmatchIndex(text)
		if match == nil || match[0] != 0 {
			return nil, ErrInvalidUpload
		}
		key := text[match[2]:match[3]]
		value := text[match[4]:match[5]]
		if _, ok := attrs[key]; ok {
			return nil, ErrInvalidUpload
		}
		attrs[key] = strings.Trim(value, `"`)
		text = text[match[1]:]
		if text != "" {
			if text[0] != ',' || len(text) == 1 {
				return nil, ErrInvalidUpload
			}
			text = text[1:]
		}
	}
	return attrs, nil
}

func validateRecordingMaster(lines []string, renditions []RecordingRendition) error {
	byPath := make(map[string]RecordingRendition)
	for _, r := range renditions {
		byPath[r.Name+"/index.m3u8"] = r
	}
	seen := make(map[string]bool)
	headers := make(map[string]bool)
	var pending map[string]string
	for _, line := range lines {
		if line == "" {
			continue
		}
		if pending != nil {
			r, ok := byPath[line]
			if !ok || seen[line] {
				return ErrInvalidUpload
			}
			seen[line] = true
			if pending["RESOLUTION"] != fmt.Sprintf("%dx%d", r.Width, r.Height) || !playlistCodecs.MatchString(pending["CODECS"]) {
				return ErrInvalidUpload
			}
			for k, v := range pending {
				switch k {
				case "BANDWIDTH", "AVERAGE-BANDWIDTH":
					n, err := strconv.ParseInt(v, 10, 64)
					if err != nil || n <= 0 {
						return ErrInvalidUpload
					}
				case "FRAME-RATE":
					n, err := strconv.ParseFloat(v, 64)
					if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || math.Abs(n-r.FrameRate) > 0.001 {
						return ErrInvalidUpload
					}
				case "RESOLUTION", "CODECS":
				default:
					return ErrInvalidUpload
				}
			}
			if pending["BANDWIDTH"] == "" {
				return ErrInvalidUpload
			}
			pending = nil
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			var err error
			pending, err = recordingAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			if err != nil {
				return err
			}
			continue
		}
		if (line != "#EXT-X-VERSION:7" && line != "#EXT-X-INDEPENDENT-SEGMENTS") || headers[line] {
			return ErrInvalidUpload
		}
		headers[line] = true
	}
	if pending != nil || len(seen) != len(renditions) || !headers["#EXT-X-VERSION:7"] || !headers["#EXT-X-INDEPENDENT-SEGMENTS"] {
		return ErrInvalidUpload
	}
	return nil
}

func validateRecordingMedia(lines []string, r RecordingRendition) ([]float64, error) {
	headers := make(map[string]bool)
	target := 0
	pending := 0.0
	ended := false
	total := 0.0
	boundaries := []float64{}
	for _, line := range lines {
		if line == "" {
			continue
		}
		if ended {
			return nil, ErrInvalidUpload
		}
		if pending > 0 {
			if line != fmt.Sprintf("seg-%06d.m4s", len(boundaries)) || !headers["#EXT-X-MAP:URI=\"init.mp4\""] || int(math.Round(pending)) > target {
				return nil, ErrInvalidUpload
			}
			tolerance := 1/r.FrameRate + 0.001
			if pending > 30+tolerance || (len(boundaries) < r.SegmentCount-1 && pending < 30-tolerance) {
				return nil, ErrInvalidUpload
			}
			total += pending
			boundaries = append(boundaries, total)
			pending = 0
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			value := strings.TrimPrefix(line, "#EXTINF:")
			if !strings.HasSuffix(value, ",") {
				return nil, ErrInvalidUpload
			}
			var err error
			pending, err = strconv.ParseFloat(strings.TrimSuffix(value, ","), 64)
			if err != nil || math.IsNaN(pending) || math.IsInf(pending, 0) || pending <= 0 {
				return nil, ErrInvalidUpload
			}
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-TARGETDURATION:") {
			if target != 0 {
				return nil, ErrInvalidUpload
			}
			var err error
			target, err = strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"))
			if err != nil || target < 1 || target > 31 {
				return nil, ErrInvalidUpload
			}
			continue
		}
		switch line {
		case "#EXT-X-VERSION:7", "#EXT-X-MEDIA-SEQUENCE:0", "#EXT-X-PLAYLIST-TYPE:VOD", "#EXT-X-INDEPENDENT-SEGMENTS", "#EXT-X-MAP:URI=\"init.mp4\"":
			if headers[line] {
				return nil, ErrInvalidUpload
			}
			headers[line] = true
		case "#EXT-X-ENDLIST":
			ended = true
		default:
			return nil, ErrInvalidUpload
		}
	}
	if !ended || pending != 0 || target == 0 || !headers["#EXT-X-VERSION:7"] || !headers["#EXT-X-MEDIA-SEQUENCE:0"] || !headers["#EXT-X-PLAYLIST-TYPE:VOD"] || len(boundaries) != r.SegmentCount || math.Abs(total-r.DurationSeconds) > 1/r.FrameRate+0.001 {
		return nil, ErrInvalidUpload
	}
	return boundaries, nil
}
