package recordingvalidation

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
)

// Absolute decode times in fragmented MP4 are per track. HLS re-muxing starts
// those clocks at zero. Match by track ID and refuse ambiguous layouts.
func fragmentClocks(data []byte) (map[uint32][]byte, error) {
	clocks := map[uint32][]byte{}
	budget := 4096
	boxes, err := fragmentBoxes(bytes.NewReader(data), int64(len(data)), &budget)
	if err != nil {
		return nil, err
	}
	for _, box := range boxes {
		if box.kind != "moof" {
			continue
		}
		trafs, err := box.children()
		if err != nil {
			return nil, err
		}
		for _, traf := range trafs {
			if traf.kind != "traf" {
				continue
			}
			entries, err := traf.children()
			if err != nil {
				return nil, err
			}
			var id uint32
			var clock []byte
			_, base, _ := box.body.Outer()
			_, offset, _ := traf.body.Outer()
			for _, entry := range entries {
				switch entry.kind {
				case "tfhd":
					var h [8]byte
					if _, err := entry.body.ReadAt(h[:], 0); err != nil || id != 0 {
						return nil, assets.ErrInvalidUpload
					}
					id = binary.BigEndian.Uint32(h[4:])
				case "tfdt":
					raw, err := entry.metadata()
					if err != nil || clock != nil || !(len(raw) == 8 && raw[0] == 0 || len(raw) == 12 && raw[0] == 1) {
						return nil, assets.ErrInvalidUpload
					}
					_, pos, _ := entry.body.Outer()
					start := base + offset + pos + 4
					clock = data[start : start+int64(len(raw)-4)]
				}
			}
			if id == 0 || clock == nil || clocks[id] != nil {
				return nil, assets.ErrInvalidUpload
			}
			clocks[id] = clock
		}
	}
	if len(clocks) != 2 {
		return nil, assets.ErrInvalidUpload
	}
	return clocks, nil
}

func alignSubstituteClocks(source, sub []byte) error {
	origins, err := fragmentClocks(source)
	if err != nil {
		return err
	}
	clocks, err := fragmentClocks(sub)
	if err != nil {
		return err
	}
	for id, out := range clocks {
		in := origins[id]
		if len(in) != len(out) {
			return assets.ErrInvalidUpload
		}
		copy(out, in)
	}
	return nil
}

type substituteWriter interface {
	PutLiveSubstitute(context.Context, string, string, int, []byte) error
}

// Spend at most one shared budget across missing profiles. Failed substitutes
// remain eligible for original-object repair and do not hold the next claim.
func prepareLiveSubstitutes(ctx context.Context, objects LiveObjects, probe PackageMediaProbe, claim postgres.RecordingLiveClaim, batch map[string]assets.RecordingLiveFragment) {
	if claim.Sequence == 0 || len(batch) == 3 {
		return
	}
	writer, ok := objects.(substituteWriter)
	if !ok {
		return
	}
	var source assets.RecordingLiveFragment
	for _, name := range []string{"720p", "1080p", "480p"} {
		if f, ok := batch[name]; ok {
			source = f
			break
		}
	}
	if source.Validation == nil {
		return
	}
	work, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	for _, target := range assets.LiveRenditions() {
		if _, ok := batch[target.Name]; ok {
			continue
		}
		start := time.Now()
		prior := claim.Snapshot.Segments[0].Renditions[target.Name]
		err := generateLiveSubstitute(work, writer, probe, claim.Capture.ID, claim.Sequence, target, prior, source)
		slog.Info("recording_live_substitute", "capture_id", claim.Capture.ID, "sequence", claim.Sequence, "rendition", target.Name, "ready", err == nil, "elapsed_ms", time.Since(start).Milliseconds())
		if err == nil {
			batch[target.Name] = assets.RecordingLiveFragment{Gap: true, Substitute: true}
		}
	}
}

func generateLiveSubstitute(ctx context.Context, writer substituteWriter, p PackageMediaProbe, id string, seq int, target assets.RecordingRendition, prior, source assets.RecordingLiveFragment) error {
	if prior.Validation == nil || source.Validation == nil {
		return assets.ErrInvalidUpload
	}
	dir, err := os.MkdirTemp(p.ScratchRoot, "hhc-live-sub-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	const maxBytes = 32 << 20
	receipt := source.Validation
	if receipt.Segment.SizeBytes > maxBytes || receipt.Init.SizeBytes > 1<<20 || prior.Validation.Init.SizeBytes > 1<<20 {
		return assets.ErrInvalidUpload
	}
	if err = checkFragmentScratch(dir, 3*maxBytes+4<<20); err != nil {
		return err
	}
	read := func(o assets.RecordingPackageObject, dst io.Writer) error {
		ok, err := p.copyLiveInit(ctx, id, o, dst)
		if err != nil {
			return err
		}
		if !ok {
			return assets.ErrCaptureMissingObjects
		}
		return nil
	}
	var segment bytes.Buffer
	if err = read(receipt.Segment, &segment); err != nil {
		return err
	}
	sourcePath := filepath.Join(dir, "source.mp4")
	err = writePackageScratch(sourcePath, func(w io.Writer) error {
		if err := read(receipt.Init, w); err != nil {
			return err
		}
		_, err := w.Write(segment.Bytes())
		return err
	})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, p.FFmpeg, "-nostdin", "-v", "error", "-xerror", "-protocol_whitelist", "file", "-threads", "2", "-i", sourcePath, "-map", "0:v:0", "-map", "0:a:0", "-vf", fmt.Sprintf("scale=%d:%d,setsar=1", target.Width, target.Height), "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-x264-params", "repeat-headers=1:sps-id=31", "-b:v", fmt.Sprint(target.VideoBitrate), "-g", "1000", "-c:a", "copy", "-f", "hls", "-hls_time", "40", "-hls_segment_type", "fmp4", "-hls_segment_filename", "sub-%d.m4s", "out.m3u8")
	cmd.Dir = dir
	if err = cmd.Run(); err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(dir, "sub-0.m4s"))
	if err != nil {
		return err
	}
	sub, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if len(sub) > maxBytes {
		return assets.ErrInvalidUpload
	}
	if err = alignSubstituteClocks(segment.Bytes(), sub); err != nil {
		return err
	}
	verify := filepath.Join(dir, "verify.mp4")
	err = writePackageScratch(verify, func(w io.Writer) error {
		if err := read(prior.Validation.Init, w); err != nil {
			return err
		}
		_, err := w.Write(sub)
		return err
	})
	if err != nil {
		return err
	}
	target.FrameRate = source.FrameRate
	actual, err := p.probeFragment(ctx, verify, target)
	if err != nil {
		return err
	}
	tolerance := 1/source.FrameRate + 0.001
	if actual.VideoFrames != source.VideoFrames || actual.AudioChannels != 2 || actual.FrameRate != source.FrameRate || math.Abs(actual.Start-source.Start) > tolerance || math.Abs(actual.End-source.End) > tolerance {
		return assets.ErrInvalidUpload
	}
	return writer.PutLiveSubstitute(ctx, id, target.Name, seq, sub)
}
