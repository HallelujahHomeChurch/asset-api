package assets

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
)

// ValidateRecordingStream checks a private R2 playback copy without staging
// the entire file on disk. Probe output must come from ffprobe reading that
// same immutable object, never from browser-provided metadata.
func ValidateRecordingStream(source io.Reader, expectedSize int64, expectedSHA256 string, probeJSON []byte) (float64, error) {
	if expectedSize <= 0 || expectedSize > RecordingMaxSizeBytes || len(expectedSHA256) != 64 {
		return 0, ErrInvalidInput
	}
	if _, err := hex.DecodeString(expectedSHA256); err != nil {
		return 0, ErrInvalidInput
	}
	var probe struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(probeJSON, &probe); err != nil {
		return 0, ErrInvalidUpload
	}
	duration, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 {
		return 0, ErrInvalidUpload
	}
	video, audio := false, false
	for _, stream := range probe.Streams {
		switch stream.CodecType {
		case "video":
			if stream.CodecName != "h264" || video {
				return 0, ErrInvalidUpload
			}
			video = true
		case "audio":
			if stream.CodecName != "aac" || audio {
				return 0, ErrInvalidUpload
			}
			audio = true
		default:
			return 0, ErrInvalidUpload
		}
	}
	if !video || !audio {
		return 0, ErrInvalidUpload
	}
	checksum := sha256.New()
	reader := io.TeeReader(io.LimitReader(source, expectedSize), checksum)
	var offset int64
	seenMoov, seenMdat := false, false
	for offset < expectedSize {
		if expectedSize-offset < 8 {
			return 0, ErrInvalidUpload
		}
		var header [16]byte
		if _, err := io.ReadFull(reader, header[:8]); err != nil {
			return 0, err
		}
		length := uint64(binary.BigEndian.Uint32(header[:4]))
		headerSize := int64(8)
		if length == 1 {
			if expectedSize-offset < 16 {
				return 0, ErrInvalidUpload
			}
			if _, err := io.ReadFull(reader, header[8:16]); err != nil {
				return 0, err
			}
			length = binary.BigEndian.Uint64(header[8:16])
			headerSize = 16
		}
		if length < uint64(headerSize) || length > uint64(expectedSize-offset) {
			return 0, ErrInvalidUpload
		}
		kind := string(header[4:8])
		if offset == 0 && kind != "ftyp" {
			return 0, ErrInvalidUpload
		}
		if kind == "moov" {
			if seenMdat || seenMoov {
				return 0, ErrInvalidUpload
			}
			seenMoov = true
		}
		if kind == "mdat" {
			if !seenMoov {
				return 0, ErrInvalidUpload
			}
			seenMdat = true
		}
		if _, err := io.CopyN(io.Discard, reader, int64(length)-headerSize); err != nil {
			return 0, err
		}
		offset += int64(length)
	}
	var extra [1]byte
	if n, err := io.ReadFull(source, extra[:]); n != 0 {
		return 0, ErrInvalidUpload
	} else if err != io.EOF {
		return 0, err
	}
	if !seenMoov || !seenMdat || !strings.EqualFold(hex.EncodeToString(checksum.Sum(nil)), expectedSHA256) {
		return 0, ErrInvalidUpload
	}
	return duration, nil
}
