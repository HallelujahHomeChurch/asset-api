package recordingprocessing

import (
	"context"
	"errors"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/recordingvalidation"
)

var ErrSourceEncode = errors.New("recording_source_encode_failed")

// EncodeSource reads the verified source through loopback and writes each
// rendition straight into the bounded output spool. No complete source or HLS
// package is materialized locally. This does not mark a package ready.
func EncodeSource(ctx context.Context, source *SourceReader, spool *OutputSpool, ffmpeg, ffprobe string) (SourcePlan, error) {
	if source == nil || spool == nil || !filepath.IsAbs(ffmpeg) || !filepath.IsAbs(ffprobe) {
		return SourcePlan{}, assets.ErrInvalidInput
	}
	data, err := recordingvalidation.ProbeCommand(ctx, ffprobe, "-v", "error", "-protocol_whitelist", "http,tcp", "-format_whitelist", "mov", "-enable_drefs", "0", "-use_absolute_path", "0", "-show_streams", "-show_format", "-of", "json", source.URL())
	if err != nil {
		return SourcePlan{}, err
	}
	plan, err := PlanBrowserSource(data)
	if err != nil {
		return SourcePlan{}, err
	}
	for _, r := range plan.Renditions {
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		err := exec.CommandContext(ctx, ffmpeg, browserEncodeArguments(source.URL(), spool.URL()+"/"+r.Name+"/", r)...).Run()
		if _, _, spoolErr := spool.Snapshot(); spoolErr != nil {
			return plan, spoolErr
		}
		if err != nil {
			if ctx.Err() != nil {
				return plan, ctx.Err()
			}
			return plan, ErrSourceEncode
		}
	}
	return plan, nil
}

// Same fixed libx264-medium hls-v1 preset as CLI; only I/O differs. Both
// renditions come from the original source, never from an already lossy output.
func browserEncodeArguments(source, output string, r assets.RecordingRendition) []string {
	i := func(v int64) string { return strconv.FormatInt(v, 10) }
	fps := strconv.FormatFloat(r.FrameRate, 'f', -1, 64)
	duration := strconv.FormatFloat(r.DurationSeconds, 'f', -1, 64)
	gop := strconv.Itoa(int(math.Ceil(r.FrameRate * 30)))
	lookahead := strconv.Itoa(min(40, max(1, int(math.Ceil(r.FrameRate*2)))))
	bframes := "3"
	if r.FrameRate < 20 {
		bframes = "0"
	}
	maxrate := r.VideoBitrate * 4 / 3
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-n",
		"-protocol_whitelist", "http,tcp", "-format_whitelist", "mov", "-enable_drefs", "0", "-use_absolute_path", "0",
		"-noautorotate", "-threads", "2", "-i", source,
		"-map", "0:V:0", "-map", "0:a:0", "-map_metadata", "-1", "-map_chapters", "-1",
		"-filter_threads", "2", "-vf", "scale=" + strconv.Itoa(r.Width) + ":" + strconv.Itoa(r.Height) + ":flags=lanczos,setsar=1",
		"-c:v", "libx264", "-preset", "medium", "-profile:v", "high", "-pix_fmt", "yuv420p", "-threads", "2", "-rc-lookahead", lookahead, "-bf", bframes,
		"-b:v", i(r.VideoBitrate), "-maxrate", i(maxrate), "-bufsize", i(2 * maxrate), "-r", fps, "-fps_mode", "cfr", "-g", gop, "-keyint_min", gop, "-sc_threshold", "0", "-flags", "+cgop", "-force_key_frames", "expr:gte(t,n_forced*30)",
		"-c:a", "aac", "-b:a", "128000", "-ar", "48000", "-ac", "2",
		// Match CLI tail padding while keeping the output duration bounded.
		"-af", "apad=whole_dur=" + duration, "-t", duration,
		"-f", "hls", "-hls_time", "30", "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4", "-hls_flags", "independent_segments", "-hls_fmp4_init_filename", "init.mp4",
		"-method", "PUT", "-hls_segment_filename", output + "seg-%06d.m4s", output + "index.m3u8",
	}
}
