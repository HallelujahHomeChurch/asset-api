package recordingvalidation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"hhc/asset-api/internal/assets"
)

const audioMaxBytes = 8 << 20

var audioSourcePrefix = regexp.MustCompile(`^recordings/packages/[a-zA-Z0-9-]{1,80}/final/[a-zA-Z0-9-]{1,80}/$`)

type audioPacketProbe struct {
	Streams []struct {
		CodecName  string `json:"codec_name"`
		CodecType  string `json:"codec_type"`
		Profile    string `json:"profile"`
		SampleRate string `json:"sample_rate"`
	} `json:"streams"`
	Packets []struct {
		PTS      string `json:"pts_time"`
		Duration string `json:"duration_time"`
	} `json:"packets"`
}

func audioBounds(data []byte) (float64, float64, error) {
	var probe audioPacketProbe
	if err := json.Unmarshal(data, &probe); err != nil {
		return 0, 0, assets.ErrInvalidUpload
	}
	if len(probe.Streams) != 1 || probe.Streams[0].CodecName != "aac" || probe.Streams[0].CodecType != "audio" || probe.Streams[0].Profile != "LC" || probe.Streams[0].SampleRate != "48000" || len(probe.Packets) == 0 || len(probe.Packets) > 2000 {
		return 0, 0, assets.ErrInvalidUpload
	}
	var start, end float64
	for i, p := range probe.Packets {
		pts, e := strconv.ParseFloat(p.PTS, 64)
		duration, d := strconv.ParseFloat(p.Duration, 64)
		if e != nil || d != nil || math.IsNaN(pts) || math.IsInf(pts, 0) || pts < 0 || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > 0.022 || i > 0 && math.Abs(pts-end) > 0.00001 {
			return 0, 0, assets.ErrInvalidUpload
		}
		if i == 0 {
			start = pts
		}
		end = pts + duration
	}
	if end-start > 31 {
		return 0, 0, assets.ErrInvalidUpload
	}
	return start, end, nil
}

// GenerateAudio losslessly remuxes validated AAC into bounded MPEG-TS fragments.
// Original sequence numbers and packet timestamps preserve broadcast range fences.
func (p PackageMediaProbe) GenerateAudio(ctx context.Context, pkg assets.RecordingPackage, attempt string, put func(context.Context, string, []byte) error) (result error) {
	if p.Objects == nil || !filepath.IsAbs(p.FFmpeg) || !filepath.IsAbs(p.FFprobe) || pkg.State != "ready" || !packageIDPattern.MatchString(attempt) || (!audioSourcePrefix.MatchString(pkg.FinalPrefix) || !strings.HasPrefix(pkg.FinalPrefix, "recordings/packages/"+pkg.ID+"/final/")) || put == nil {
		return assets.ErrInvalidInput
	}
	if _, err := assets.ValidateRecordingInventory(pkg.Inventory); err != nil {
		return err
	}
	r := pkg.Inventory.Renditions[0]
	for _, candidate := range pkg.Inventory.Renditions {
		if candidate.VideoBitrate < r.VideoBitrate {
			r = candidate
		}
	}
	objects := map[string]assets.RecordingPackageObject{}
	for _, o := range pkg.Inventory.Objects {
		objects[o.Path] = o
	}
	dir, err := os.MkdirTemp(p.ScratchRoot, "hhc-audio-")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(dir)) }()
	prefix := pkg.FinalPrefix + "audio/" + assets.RecordingAudioVersion + "/" + attempt + "/"
	var playlist strings.Builder
	fmt.Fprintf(&playlist, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:31\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	var previousEnd, firstStart float64
	for i := 0; i < r.SegmentCount; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		var disk syscall.Statfs_t
		if err := syscall.Statfs(dir, &disk); err != nil {
			return err
		}
		required := objects[r.Name+"/init.mp4"].SizeBytes + objects[fmt.Sprintf("%s/seg-%06d.m4s", r.Name, i)].SizeBytes + audioMaxBytes
		if required <= audioMaxBytes || uint64(required)/uint64(disk.Bsize)+1 > disk.Bavail {
			return errors.New("insufficient audio scratch capacity")
		}
		input, output := filepath.Join(dir, "input.mp4"), filepath.Join(dir, "output.ts")
		f, err := os.OpenFile(input, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		copyErr := func() error {
			for _, name := range []string{r.Name + "/init.mp4", fmt.Sprintf("%s/seg-%06d.m4s", r.Name, i)} {
				o, ok := objects[name]
				if !ok || o.SizeBytes <= 0 || o.SizeBytes > assets.RecordingObjectMaxBytes {
					return assets.ErrInvalidUpload
				}
				body, err := p.Objects.Open(ctx, pkg.FinalPrefix+name)
				if err != nil {
					return err
				}
				hash := sha256.New()
				n, e := io.Copy(io.MultiWriter(f, hash), io.LimitReader(body, o.SizeBytes+1))
				e = errors.Join(e, body.Close())
				if e != nil {
					return e
				}
				if n != o.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != o.SHA256 {
					return assets.ErrInvalidUpload
				}
			}
			return nil
		}()
		if err := errors.Join(copyErr, f.Close()); err != nil {
			return err
		}
		segmentCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err = exec.CommandContext(segmentCtx, p.FFmpeg, "-nostdin", "-y", "-v", "error", "-xerror", "-protocol_whitelist", "file", "-enable_drefs", "0", "-use_absolute_path", "0", "-copyts", "-i", input, "-map", "0:a:0", "-vn", "-c:a", "copy", "-mpegts_copyts", "1", "-muxdelay", "0", "-fs", strconv.Itoa(audioMaxBytes), "-f", "mpegts", output).Run()
		if err != nil {
			cancel()
			return err
		}
		data, err := ProbeCommand(segmentCtx, p.FFprobe, "-v", "error", "-protocol_whitelist", "file", "-show_streams", "-show_packets", "-show_entries", "stream=codec_name,codec_type,profile,sample_rate:packet=pts_time,duration_time", "-of", "json", output)
		if err != nil {
			cancel()
			return err
		}
		start, end, err := audioBounds(data)
		if err != nil {
			cancel()
			return err
		}
		if i == 0 {
			firstStart = start
		} else if math.Abs(start-previousEnd) > 0.00001 {
			cancel()
			return assets.ErrInvalidUpload
		}
		previousEnd = end
		err = exec.CommandContext(segmentCtx, p.FFmpeg, "-nostdin", "-v", "error", "-xerror", "-protocol_whitelist", "file", "-i", output, "-map", "0:a:0", "-f", "null", "-").Run()
		cancel()
		if err != nil {
			return err
		}
		out, err := os.Open(output)
		if err != nil {
			return err
		}
		payload, err := io.ReadAll(io.LimitReader(out, audioMaxBytes+1))
		err = errors.Join(err, out.Close())
		if err != nil {
			return err
		}
		if len(payload) == 0 || len(payload) >= audioMaxBytes {
			return assets.ErrInvalidUpload
		}
		name := fmt.Sprintf("seg-%06d.ts", i)
		if err := put(ctx, prefix+name, payload); err != nil {
			return err
		}
		fmt.Fprintf(&playlist, "#EXTINF:%.6f,\n%s\n", end-start, name)
	}
	// Permit only the AAC frame rounding already present in the source package.
	if math.Abs((previousEnd-firstStart)-r.DurationSeconds) > 0.1 {
		return assets.ErrInvalidUpload
	}
	playlist.WriteString("#EXT-X-ENDLIST\n")
	return put(ctx, prefix+"index.m3u8", []byte(playlist.String()))
}
