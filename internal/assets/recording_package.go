package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"
)

const (
	RecordingSourceMaxBytes     int64 = 50_000_000_000
	RecordingPackageMaxBytes    int64 = 10_000_000_000
	RecordingObjectMaxBytes     int64 = 128 << 20
	RecordingInventoryMaxBytes        = 8 << 20
	RecordingPlaylistMaxBytes   int64 = 1 << 20
	RecordingPackageMaxObjects        = 10_000
	RecordingMaxDurationSeconds       = 12 * 60 * 60
)

var (
	ErrRecordingSourceTooLarge          = errors.New("source_too_large")
	ErrRecordingPackageTooLarge         = errors.New("package_too_large")
	ErrRecordingPackageEstimateTooLarge = errors.New("package_size_estimate_exceeded")
	presetVersionPattern                = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
)

// RecordingPackageInventory describes declared content, never proof of ready.
// Validation workers must verify final bytes and the actual media timeline.
type RecordingPackageInventory struct {
	SchemaVersion   int                      `json:"schemaVersion"`
	PresetVersion   string                   `json:"presetVersion"`
	Objects         []RecordingPackageObject `json:"objects"`
	Renditions      []RecordingRendition     `json:"renditions"`
	InventoryDigest string                   `json:"inventoryDigest,omitempty"`
}

type RecordingPackageObject struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

type RecordingRendition struct {
	Name            string  `json:"name"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	FrameRate       float64 `json:"frameRate"`
	VideoBitrate    int64   `json:"videoBitrate"`
	AudioBitrate    int64   `json:"audioBitrate"`
	DurationSeconds float64 `json:"durationSeconds"`
	SegmentCount    int     `json:"segmentCount"`
}

// DecodeRecordingInventory bounds the request before parsing and requires the
// caller's canonical digest. The digest is integrity metadata, not identity.
func DecodeRecordingInventory(reader io.Reader) (RecordingPackageInventory, error) {
	var inv RecordingPackageInventory
	data, err := io.ReadAll(io.LimitReader(reader, RecordingInventoryMaxBytes+1))
	if err != nil {
		return inv, err
	}
	if len(data) > RecordingInventoryMaxBytes {
		return inv, fmt.Errorf("%w: inventory size", ErrInvalidInput)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inv); err != nil {
		return inv, fmt.Errorf("%w: inventory JSON", ErrInvalidInput)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return inv, fmt.Errorf("%w: trailing JSON", ErrInvalidInput)
	}
	if inv.InventoryDigest == "" {
		return inv, fmt.Errorf("%w: missing inventory digest", ErrInvalidInput)
	}
	if _, err := ValidateRecordingInventory(inv); err != nil {
		return inv, err
	}
	return inv, nil
}

func ValidateRecordingSourceSize(size int64) error {
	if size <= 0 {
		return ErrInvalidInput
	}
	if size > RecordingSourceMaxBytes {
		return ErrRecordingSourceTooLarge
	}
	return nil
}

// EstimateRecordingPackageSize reserves 3% for mux/control overhead, but the
// resulting package must still be checked against actual cumulative bytes.
func EstimateRecordingPackageSize(duration float64, videoBitrates []int64) (int64, error) {
	if math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > RecordingMaxDurationSeconds || len(videoBitrates) < 1 || len(videoBitrates) > 2 {
		return 0, ErrInvalidInput
	}
	var bitsPerSecond int64
	for _, bitrate := range videoBitrates {
		if bitrate < 500_000 || bitrate > 8_000_000 {
			return 0, ErrInvalidInput
		}
		bitsPerSecond += bitrate + 128_000
	}
	size := int64(math.Ceil(duration * float64(bitsPerSecond) / 8 * 1.03))
	if size > RecordingPackageMaxBytes {
		return size, ErrRecordingPackageEstimateTooLarge
	}
	return size, nil
}

// RecordingInventoryDigest uses UTF-8 encoding/json bytes of this struct with
// inventoryDigest omitted; objects and renditions are sorted by path/name.
// Do not mutate the caller's slices when computing the digest.
func RecordingInventoryDigest(inv RecordingPackageInventory) (string, error) {
	inv.InventoryDigest = ""
	inv.Objects = slices.Clone(inv.Objects)
	inv.Renditions = slices.Clone(inv.Renditions)
	slices.SortFunc(inv.Objects, func(a, b RecordingPackageObject) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(inv.Renditions, func(a, b RecordingRendition) int { return strings.Compare(a.Name, b.Name) })
	data, err := json.Marshal(inv)
	if err != nil {
		return "", fmt.Errorf("%w: inventory serialization", ErrInvalidInput)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// ValidateRecordingInventory checks the complete declared graph and returns
// total bytes including package.json. Metadata is untrusted until probed.
func ValidateRecordingInventory(inv RecordingPackageInventory) (int64, error) {
	invalid := func(reason string) (int64, error) { return 0, fmt.Errorf("%w: %s", ErrInvalidInput, reason) }
	if inv.SchemaVersion != 1 || !presetVersionPattern.MatchString(inv.PresetVersion) || len(inv.Objects) > RecordingPackageMaxObjects || len(inv.Renditions) < 1 || len(inv.Renditions) > 2 {
		return invalid("schema or limits")
	}
	expected := map[string]bool{"master.m3u8": true}
	seenRenditions := make(map[string]bool)
	var first RecordingRendition
	for n, rendition := range inv.Renditions {
		maxWidth, maxHeight := 1280, 720
		if rendition.Name == "1080p" {
			maxWidth, maxHeight = 1920, 1080
		} else if rendition.Name != "720p" {
			return invalid("rendition name")
		}
		if seenRenditions[rendition.Name] {
			return invalid("duplicate rendition")
		}
		seenRenditions[rendition.Name] = true
		if rendition.Width <= 0 || rendition.Width > maxWidth || rendition.Width%2 != 0 || rendition.Height <= 0 || rendition.Height > maxHeight || rendition.Height%2 != 0 || math.IsNaN(rendition.FrameRate) || math.IsInf(rendition.FrameRate, 0) || rendition.FrameRate <= 0 || rendition.FrameRate > 30 || math.IsNaN(rendition.DurationSeconds) || math.IsInf(rendition.DurationSeconds, 0) || rendition.DurationSeconds <= 0 || rendition.DurationSeconds > RecordingMaxDurationSeconds || rendition.SegmentCount != int(math.Ceil(rendition.DurationSeconds/30)) || rendition.AudioBitrate != 128_000 || rendition.VideoBitrate < 500_000 || rendition.VideoBitrate > 8_000_000 {
			return invalid("rendition metadata")
		}
		if n == 0 {
			first = rendition
		} else if rendition.FrameRate != first.FrameRate || math.Abs(rendition.DurationSeconds-first.DurationSeconds) > 1/first.FrameRate {
			return invalid("rendition timeline")
		}
		expected[rendition.Name+"/index.m3u8"] = true
		expected[rendition.Name+"/init.mp4"] = true
		for segment := 0; segment < rendition.SegmentCount; segment++ {
			expected[fmt.Sprintf("%s/seg-%06d.m4s", rendition.Name, segment)] = true
		}
	}
	if !seenRenditions["720p"] || len(inv.Objects) != len(expected) {
		return invalid("missing low rendition or object")
	}
	if len(inv.Renditions) == 2 {
		var low, high RecordingRendition
		for _, r := range inv.Renditions {
			if r.Name == "720p" {
				low = r
			} else {
				high = r
			}
		}
		if high.VideoBitrate < low.VideoBitrate || high.Height <= low.Height {
			return invalid("rendition ordering")
		}
	}
	data, err := json.Marshal(inv)
	if err != nil || len(data) > RecordingInventoryMaxBytes {
		return invalid("inventory size or serialization")
	}
	total := int64(len(data))
	for _, object := range inv.Objects {
		if !expected[object.Path] || object.SizeBytes <= 0 || object.SizeBytes > RecordingObjectMaxBytes {
			return invalid("object path, duplicate or size")
		}
		expected[object.Path] = false
		if strings.HasSuffix(object.Path, ".m3u8") && object.SizeBytes > RecordingPlaylistMaxBytes {
			return invalid("playlist size")
		}
		decoded, err := hex.DecodeString(object.SHA256)
		if err != nil || len(decoded) != 32 || strings.ToLower(object.SHA256) != object.SHA256 {
			return invalid("checksum")
		}
		total += object.SizeBytes
		if total > RecordingPackageMaxBytes {
			return 0, ErrRecordingPackageTooLarge
		}
	}
	if inv.InventoryDigest != "" {
		digest, err := RecordingInventoryDigest(inv)
		if err != nil || digest != inv.InventoryDigest {
			return invalid("inventory digest")
		}
	}
	return total, nil
}
