package recordingprocessing

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"hhc/asset-api/internal/assets"
)

type SourcePlan struct {
	Renditions     []assets.RecordingRendition
	EstimatedBytes int64
}

// PlanBrowserSource uses the same hls-v1 defaults as CLI PlanSource. Browser
// input is MP4 only; unsupported HDR/rotation is rejected instead of silently
// tone-mapping, upscaling or dropping audio. Input must come from bounded ffprobe.
func PlanBrowserSource(data []byte) (SourcePlan, error) {
	var probe struct {
		Streams []struct {
			Type        string `json:"codec_type"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			SAR         string `json:"sample_aspect_ratio"`
			Rate        string `json:"avg_frame_rate"`
			Transfer    string `json:"color_transfer"`
			Disposition struct {
				AttachedPicture int `json:"attached_pic"`
			} `json:"disposition"`
			SideData []struct {
				Rotation int `json:"rotation"`
			} `json:"side_data_list"`
		} `json:"streams"`
		Format struct {
			Name     string `json:"format_name"`
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if len(data) > 1<<20 || json.Unmarshal(data, &probe) != nil || len(probe.Streams) > 64 || strings.Split(probe.Format.Name, ",")[0] != "mov" {
		return SourcePlan{}, assets.ErrInvalidUpload
	}
	duration, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil || !positiveFinite(duration) || duration > assets.RecordingMaxDurationSeconds {
		return SourcePlan{}, assets.ErrInvalidUpload
	}
	video, hasAudio := -1, false
	for i, stream := range probe.Streams {
		if stream.Type == "audio" {
			hasAudio = true
		}
		if stream.Type == "video" && stream.Disposition.AttachedPicture == 0 && video < 0 {
			video = i
		}
	}
	if video < 0 || !hasAudio {
		return SourcePlan{}, assets.ErrInvalidUpload
	}
	stream := probe.Streams[video]
	sar, rate := sourceRatio(stream.SAR, ":"), sourceRatio(stream.Rate, "/")
	if stream.Width <= 0 || stream.Width > 8192 || stream.Height <= 0 || stream.Height > 8192 || !positiveFinite(sar) || sar > 16 || !positiveFinite(rate) {
		return SourcePlan{}, assets.ErrInvalidUpload
	}
	for _, side := range stream.SideData {
		if side.Rotation != 0 {
			return SourcePlan{}, assets.ErrInvalidUpload
		}
	}
	switch stream.Transfer {
	case "", "unknown", "unspecified", "bt709", "smpte170m", "smpte240m", "gamma22", "gamma28", "iec61966-2-1":
	default:
		return SourcePlan{}, assets.ErrInvalidUpload
	}
	width, height := float64(stream.Width)*sar, float64(stream.Height)
	plan := SourcePlan{}
	var bitrates []int64
	for _, limit := range []struct {
		name          string
		width, height float64
		bitrate       int64
	}{{"720p", 1280, 720, 1500000}, {"1080p", 1920, 1080, 3000000}} {
		scale := min(1, limit.width/width, limit.height/height)
		w, h := int(math.Floor(width*scale/2))*2, int(math.Floor(height*scale/2))*2
		if w < 2 || h < 2 {
			return SourcePlan{}, assets.ErrInvalidUpload
		}
		if len(plan.Renditions) > 0 && h <= plan.Renditions[0].Height {
			break
		}
		plan.Renditions = append(plan.Renditions, assets.RecordingRendition{Name: limit.name, Width: w, Height: h, FrameRate: min(30, rate), VideoBitrate: limit.bitrate, AudioBitrate: 128000, DurationSeconds: duration, SegmentCount: int(math.Ceil(duration / 30))})
		bitrates = append(bitrates, limit.bitrate)
	}
	plan.EstimatedBytes, err = assets.EstimateRecordingPackageSize(duration, bitrates)
	return plan, err
}

func positiveFinite(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }
func sourceRatio(value, separator string) float64 {
	parts := strings.Split(value, separator)
	if len(parts) != 2 {
		return 0
	}
	n, e1 := strconv.ParseFloat(parts[0], 64)
	d, e2 := strconv.ParseFloat(parts[1], 64)
	if e1 != nil || e2 != nil || !positiveFinite(n) || !positiveFinite(d) {
		return 0
	}
	return n / d
}
