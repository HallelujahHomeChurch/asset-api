package recordingvalidation

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"hhc/asset-api/internal/assets"
)

type PackageMediaProbe struct {
	Objects                      PackageObjects
	FFmpeg, FFprobe, ScratchRoot string
	OnProgress                   func(assets.RecordingProcessingProgress)
}

// Validate decodes bounded init+fragments. Untrusted media cannot
// make ffmpeg open network protocols; scratch never contains the full package.
func (p PackageMediaProbe) Validate(ctx context.Context, inv assets.RecordingPackageInventory, prefix string) (result error) {
	if p.Objects == nil || !filepath.IsAbs(p.FFmpeg) || !filepath.IsAbs(p.FFprobe) {
		return assets.ErrInvalidInput
	}
	sizes := map[string]int64{}
	for _, o := range inv.Objects {
		sizes[o.Path] = o.SizeBytes
	}
	body, err := p.Objects.Open(ctx, prefix+"master.m3u8")
	if err != nil {
		return err
	}
	master, err := io.ReadAll(io.LimitReader(body, assets.RecordingPlaylistMaxBytes+1))
	err = errors.Join(err, body.Close())
	if err != nil {
		return err
	}
	read := func(ctx context.Context, name string, w io.Writer) error {
		size := sizes[name]
		if size <= 0 || size > assets.RecordingObjectMaxBytes {
			return assets.ErrInvalidUpload
		}
		body, err := p.Objects.Open(ctx, prefix+name)
		if err != nil {
			return err
		}
		n, readErr := io.Copy(w, io.LimitReader(body, size+1))
		if err := errors.Join(readErr, body.Close()); err != nil {
			return err
		}
		if n != size {
			return assets.ErrInvalidUpload
		}
		return nil
	}
	return p.validateMedia(ctx, inv, master, read, read)
}

func (p PackageMediaProbe) probeFragment(ctx context.Context, path string, r assets.RecordingRendition) (segmentProbe, error) {
	data, err := ProbeCommand(ctx, p.FFprobe, "-v", "error", "-protocol_whitelist", "file", "-enable_drefs", "0", "-use_absolute_path", "0", "-show_data", "-show_streams", "-show_packets", "-show_entries", "stream=index,codec_type,codec_name,width,height,pix_fmt,sample_rate,channels,profile,r_frame_rate,sample_aspect_ratio,extradata:packet=stream_index,pts_time,duration_time,flags", "-of", "json", path)
	if err != nil {
		return segmentProbe{}, err
	}
	actual, err := validatePackageSegmentProbe(data, r)
	if err != nil {
		return segmentProbe{}, fmt.Errorf("segment stream or timeline: %w", err)
	}
	first, err := ProbeCommand(ctx, p.FFprobe, "-v", "error", "-protocol_whitelist", "file", "-enable_drefs", "0", "-use_absolute_path", "0", "-select_streams", "v:0", "-read_intervals", "%+#1", "-show_packets", "-show_data", "-show_entries", "packet=flags,data", "-of", "json", path)
	if err != nil {
		return segmentProbe{}, err
	}
	if err := validateFirstIDRPacket(first); err != nil {
		return segmentProbe{}, fmt.Errorf("segment IDR: %w", err)
	}
	decodeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = exec.CommandContext(decodeCtx, p.FFmpeg, "-nostdin", "-v", "error", "-xerror", "-protocol_whitelist", "file", "-enable_drefs", "0", "-use_absolute_path", "0", "-threads", "2", "-i", path, "-map", "0:v:0", "-map", "0:a:0", "-f", "null", "-").Run()
	deadlineErr := decodeCtx.Err()
	cancel()
	if err != nil {
		if deadlineErr != nil {
			return segmentProbe{}, deadlineErr
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() < 0 {
			return segmentProbe{}, errors.New("media decoder unavailable or terminated")
		}
		return segmentProbe{}, assets.ErrInvalidUpload
	}
	return actual, nil
}

// ProbeCommand shares the existing bounded, deadline-limited metadata runner
// with source processing. Callers must supply their restricted protocol/demuxer
// arguments and only a server-owned source URL or private fragment path.
func ProbeCommand(ctx context.Context, binary string, args ...string) ([]byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, binary, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, (8<<20)+1))
	if readErr != nil || len(data) > 8<<20 {
		cancel()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if probeCtx.Err() != nil {
		return nil, probeCtx.Err()
	}
	if readErr != nil || waitErr != nil || len(data) > 8<<20 {
		return nil, assets.ErrInvalidUpload
	}
	return data, nil
}

type segmentProbe struct {
	AudioChannels int
	Start, End    float64
	Codecs        string
}

// Only bounded ffprobe output from a local init+fragment is accepted here.
func validatePackageSegmentProbe(data []byte, r assets.RecordingRendition) (segmentProbe, error) {
	stage := "streams"
	invalid := func() (segmentProbe, error) {
		return segmentProbe{}, fmt.Errorf("%w: %s", assets.ErrInvalidUpload, stage)
	}
	var output struct {
		Streams []struct {
			Index       int    `json:"index"`
			Codec       string `json:"codec_name"`
			Type        string `json:"codec_type"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			PixelFormat string `json:"pix_fmt"`
			Aspect      string `json:"sample_aspect_ratio"`
			Rate        string `json:"r_frame_rate"`
			Extra       string `json:"extradata"`
			Profile     string `json:"profile"`
			SampleRate  string `json:"sample_rate"`
			Channels    int    `json:"channels"`
		} `json:"streams"`
		Packets []struct {
			Stream   int    `json:"stream_index"`
			PTS      string `json:"pts_time"`
			Duration string `json:"duration_time"`
			Flags    string `json:"flags"`
		} `json:"packets"`
	}
	if len(data) > 8<<20 || json.Unmarshal(data, &output) != nil || len(output.Streams) != 2 || len(output.Packets) == 0 || len(output.Packets) > 4096 || r.FrameRate <= 0 || r.FrameRate > 30 {
		return invalid()
	}
	video, audio := -1, -1
	audioChannels := 0
	codecs := ""
	for _, s := range output.Streams {
		switch s.Type {
		case "video":
			parts := strings.Split(s.Rate, "/")
			if len(parts) != 2 {
				return invalid()
			}
			n, e1 := strconv.ParseFloat(parts[0], 64)
			d, e2 := strconv.ParseFloat(parts[1], 64)
			if video != -1 || s.Index < 0 || s.Codec != "h264" || s.Width != r.Width || s.Height != r.Height || s.PixelFormat != "yuv420p" || s.Aspect != "1:1" || e1 != nil || e2 != nil || !finite(n) || !finite(d) || d <= 0 || math.Abs(n/d-r.FrameRate) > 0.001 {
				return invalid()
			}
			var err error
			codecs, err = avccCodecs(s.Extra)
			if err != nil {
				return invalid()
			}
			video = s.Index
		case "audio":
			if audio != -1 || s.Index < 0 || s.Codec != "aac" || s.Profile != "LC" || s.SampleRate != "48000" || s.Channels < 1 || s.Channels > 2 {
				return invalid()
			}
			audio = s.Index
			audioChannels = s.Channels
		default:
			return invalid()
		}
	}
	if video < 0 || audio < 0 || video == audio {
		return invalid()
	}
	type packet struct {
		pts, duration float64
		key           bool
	}
	stage = "packet fields"
	tracks := map[int][]packet{video: nil, audio: nil}
	for _, p := range output.Packets {
		pts, e1 := strconv.ParseFloat(p.PTS, 64)
		duration, e2 := strconv.ParseFloat(p.Duration, 64)
		missingDuration := p.Stream == audio && p.Duration == ""
		if p.Stream == video && p.Duration == "" {
			// ffprobe 5.1 can omit early fMP4 video durations before detecting
			// the frame rate. The verified CFR interval is checked against every
			// actual PTS below; full decode and total duration checks still apply.
			duration = 1 / r.FrameRate
			e2 = nil
		}
		if missingDuration {
			duration = 0
			e2 = nil
		}
		if _, ok := tracks[p.Stream]; !ok || e1 != nil || e2 != nil || !finite(pts) || !finite(duration) || duration <= 0 && !missingDuration || duration > 1 {
			return invalid()
		}
		tracks[p.Stream] = append(tracks[p.Stream], packet{pts, duration, strings.Contains(p.Flags, "K")})
	}
	for stream, packets := range tracks {
		stage = "packet continuity"
		if len(packets) == 0 || stream == video && (len(packets) > 1000 || !packets[0].key) || stream == audio && len(packets) > 2000 {
			return invalid()
		}
		slices.SortFunc(packets, func(a, b packet) int {
			if a.pts < b.pts {
				return -1
			}
			if a.pts > b.pts {
				return 1
			}
			return 0
		})
		for i, p := range packets {
			// Older ffprobe omits the first AAC packet duration in a standalone
			// fragment. Infer only that packet from the next actual timestamp,
			// bounded to an AAC-LC 960/1024-sample frame at the verified 48kHz.
			if p.duration == 0 {
				if stream != audio || i != 0 || len(packets) < 2 {
					return invalid()
				}
				p.duration = packets[1].pts - p.pts
				if math.Abs(p.duration-1024.0/48000) > 0.000002 && math.Abs(p.duration-960.0/48000) > 0.000002 {
					return invalid()
				}
				packets[i].duration = p.duration
			}
			if stream == video && math.Abs(p.duration-1/r.FrameRate) > 0.0002 {
				return invalid()
			}
			if i > 0 && (p.pts <= packets[i-1].pts || math.Abs(p.pts-packets[i-1].pts-packets[i-1].duration) > 0.0002) {
				return invalid()
			}
		}
	}
	v, a := tracks[video], tracks[audio]
	end := v[len(v)-1].pts + v[len(v)-1].duration
	if math.Abs(v[0].pts-a[0].pts) > 0.1 || math.Abs(end-a[len(a)-1].pts-a[len(a)-1].duration) > 0.1 || end-v[0].pts > 30+1/r.FrameRate+0.001 {
		return invalid()
	}
	return segmentProbe{Start: v[0].pts, End: end, Codecs: codecs, AudioChannels: audioChannels}, nil
}

func avccCodecs(extra string) (string, error) {
	// AVCC starts with configurationVersion, profile, compatibility, level.
	first := strings.Split(strings.TrimSpace(extra), "\n")[0]
	fields := strings.Fields(first)
	if len(fields) < 4 || fields[0] != "00000000:" || len(fields[3]) < 2 {
		return "", assets.ErrInvalidUpload
	}
	avcc, err := hex.DecodeString(fields[1] + fields[2] + fields[3][:2])
	if err != nil || len(avcc) != 5 || avcc[0] != 1 || avcc[4] != 0xff {
		return "", assets.ErrInvalidUpload
	}
	return "avc1." + hex.EncodeToString(avcc[1:4]) + ",mp4a.40.2", nil
}

// InitCodecs derives the master playlist's codec labels from the actual encoded
// initialization object. Full packet/IDR/decode checks still run in Validate.
func (p PackageMediaProbe) InitCodecs(ctx context.Context, prefix, name string, size int64) (codecs string, result error) {
	if p.Objects == nil || !filepath.IsAbs(p.FFprobe) || (name != "480p" && name != "720p" && name != "1080p") || size <= 0 || size > assets.RecordingObjectMaxBytes {
		return "", assets.ErrInvalidInput
	}
	dir, err := os.MkdirTemp(p.ScratchRoot, "hhc-init-")
	if err != nil {
		return "", err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(dir)) }()
	var disk syscall.Statfs_t
	if err := syscall.Statfs(dir, &disk); err != nil {
		return "", err
	}
	if disk.Bsize <= 0 || uint64(size+(64<<20))/uint64(disk.Bsize)+1 > disk.Bavail {
		return "", errors.New("insufficient init scratch capacity")
	}
	path := filepath.Join(dir, "init.mp4")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	body, err := p.Objects.Open(ctx, prefix+name+"/init.mp4")
	if err != nil {
		file.Close()
		return "", err
	}
	n, err := io.CopyBuffer(file, io.LimitReader(body, size+1), make([]byte, 64<<10))
	if err = errors.Join(err, body.Close(), file.Close()); err != nil {
		return "", err
	}
	if n != size {
		return "", assets.ErrInvalidUpload
	}
	data, err := ProbeCommand(ctx, p.FFprobe, "-v", "error", "-protocol_whitelist", "file", "-format_whitelist", "mov", "-enable_drefs", "0", "-use_absolute_path", "0", "-show_streams", "-show_data", "-show_entries", "stream=codec_type,codec_name,profile,extradata", "-of", "json", path)
	if err != nil {
		return "", err
	}
	var output struct {
		Streams []struct {
			Type    string `json:"codec_type"`
			Codec   string `json:"codec_name"`
			Profile string `json:"profile"`
			Extra   string `json:"extradata"`
		} `json:"streams"`
	}
	if json.Unmarshal(data, &output) != nil || len(output.Streams) != 2 {
		return "", assets.ErrInvalidUpload
	}
	video, audio := false, false
	for _, stream := range output.Streams {
		switch {
		case stream.Type == "video" && stream.Codec == "h264" && !video:
			codecs, err = avccCodecs(stream.Extra)
			if err != nil {
				return "", err
			}
			video = true
		case stream.Type == "audio" && stream.Codec == "aac" && !audio:
			// An init-only file has no decoded packets, so ffprobe may omit
			// profile. AudioSpecificConfig's first five bits are the object
			// type: 2 is AAC-LC. Full decode validates the packets afterwards.
			fields := strings.Fields(strings.Split(strings.TrimSpace(stream.Extra), "\n")[0])
			if len(fields) < 2 || fields[0] != "00000000:" || len(fields[1]) < 2 {
				return "", assets.ErrInvalidUpload
			}
			first, err := strconv.ParseUint(fields[1][:2], 16, 8)
			if err != nil || first>>3 != 2 {
				return "", assets.ErrInvalidUpload
			}
			audio = true
		default:
			return "", assets.ErrInvalidUpload
		}
	}
	if !video || !audio {
		return "", assets.ErrInvalidUpload
	}
	return codecs, nil
}

func finite(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) }

func validateFirstIDRPacket(data []byte) error {
	var output struct {
		Packets []struct {
			Flags string `json:"flags"`
			Data  string `json:"data"`
		} `json:"packets"`
	}
	if len(data) > 8<<20 || json.Unmarshal(data, &output) != nil || len(output.Packets) != 1 || !strings.Contains(output.Packets[0].Flags, "K") {
		return assets.ErrInvalidUpload
	}
	var packet []byte
	for _, line := range strings.Split(strings.TrimSpace(output.Packets[0].Data), "\n") {
		offset, rest, ok := strings.Cut(line, ": ")
		if !ok {
			return assets.ErrInvalidUpload
		}
		n, err := strconv.ParseUint(offset, 16, 32)
		if err != nil || n != uint64(len(packet)) {
			return assets.ErrInvalidUpload
		}
		hexData, _, _ := strings.Cut(rest, "  ")
		decoded, err := hex.DecodeString(strings.ReplaceAll(hexData, " ", ""))
		if err != nil {
			return assets.ErrInvalidUpload
		}
		packet = append(packet, decoded...)
	}
	idr := false
	seenVCL := false
	for len(packet) > 0 {
		if len(packet) < 5 {
			return assets.ErrInvalidUpload
		}
		size := uint64(binary.BigEndian.Uint32(packet[:4]))
		packet = packet[4:]
		if size == 0 || size > uint64(len(packet)) {
			return assets.ErrInvalidUpload
		}
		kind := packet[0] & 0x1f
		if kind >= 1 && kind <= 5 && !seenVCL {
			seenVCL = true
			idr = kind == 5
		}
		packet = packet[int(size):]
	}
	if !idr {
		return assets.ErrInvalidUpload
	}
	return nil
}
