package recordingvalidation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
)

type PreviewObjects interface {
	PackageObjects
	PutPackagePreview(context.Context, string, []byte) error
}

func RunPackagePreview(ctx context.Context, repo *postgres.RecordingPackageStore, objects PreviewObjects, probe PackageMediaProbe) (bool, error) {
	c, err := repo.ClaimPackagePreview(ctx)
	if errors.Is(err, assets.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, RunProcessingClaim(ctx, func(ctx context.Context) error { return repo.HeartbeatPackagePreview(ctx, c.Package.ID, c.ClaimID) }, func(ctx context.Context) error {
		return probe.GeneratePreviews(ctx, c.Package, c.ClaimID, objects.PutPackagePreview)
	}, func(ctx context.Context, ready bool, _ string) error {
		return repo.FinishPackagePreview(ctx, c.Package.ID, c.ClaimID, ready, func(ctx context.Context) error {
			return objects.PutPackagePreview(ctx, c.Package.FinalPrefix+"previews/current.json", []byte(fmt.Sprintf(`{"attempt":"%s"}`, c.ClaimID)))
		})
	}, 30*time.Second)
}

// GeneratePreviews reads only validated immutable media, one init+fragment
// at a time. Reset PTS before selecting frames: later fMP4 fragments retain
// the full recording timeline. Partial tiles are never referenced by cues.
func (p PackageMediaProbe) GeneratePreviews(ctx context.Context, pkg assets.RecordingPackage, attempt string, put func(context.Context, string, []byte) error) (result error) {
	if p.Objects == nil || !filepath.IsAbs(p.FFmpeg) || !filepath.IsAbs(p.FFprobe) || pkg.State != "ready" || !packageIDPattern.MatchString(pkg.ID) || !packageIDPattern.MatchString(attempt) || put == nil {
		return assets.ErrInvalidInput
	}
	prefixParts := strings.Split(pkg.FinalPrefix, "/")
	if len(prefixParts) != 6 || prefixParts[0] != "recordings" || prefixParts[1] != "packages" || prefixParts[2] != pkg.ID || prefixParts[3] != "final" || !packageIDPattern.MatchString(prefixParts[4]) || prefixParts[5] != "" {
		return assets.ErrInvalidInput
	}
	if _, err := assets.ValidateRecordingInventory(pkg.Inventory); err != nil {
		return err
	}
	r := pkg.Inventory.Renditions[0]
	for _, candidate := range pkg.Inventory.Renditions {
		if candidate.Width < r.Width {
			r = candidate
		}
	}
	dir, err := os.MkdirTemp(p.ScratchRoot, "hhc-preview-")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(dir)) }()
	sizes := map[string]int64{}
	for _, o := range pkg.Inventory.Objects {
		sizes[o.Path] = o.SizeBytes
	}
	outPrefix := pkg.FinalPrefix + "previews/" + attempt + "/"
	var vtt strings.Builder
	vtt.WriteString("WEBVTT\n\n")
	for i := 0; i < r.SegmentCount; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		input := filepath.Join(dir, "fragment.mp4")
		output := filepath.Join(dir, "sprite.jpg")
		var disk syscall.Statfs_t
		if err := syscall.Statfs(dir, &disk); err != nil {
			return err
		}
		required := sizes[r.Name+"/init.mp4"] + sizes[fmt.Sprintf("%s/seg-%06d.m4s", r.Name, i)] + (64 << 20)
		if required <= 64<<20 || required > 2*assets.RecordingObjectMaxBytes+(64<<20) || uint64(required)/uint64(disk.Bsize)+1 > disk.Bavail {
			return errors.New("insufficient preview scratch capacity")
		}
		f, err := os.OpenFile(input, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		copyErr := func() error {
			for _, name := range []string{r.Name + "/init.mp4", fmt.Sprintf("%s/seg-%06d.m4s", r.Name, i)} {
				size := sizes[name]
				if size <= 0 || size > assets.RecordingObjectMaxBytes {
					return assets.ErrInvalidUpload
				}
				body, err := p.Objects.Open(ctx, pkg.FinalPrefix+name)
				if err != nil {
					return err
				}
				n, err := io.Copy(f, io.LimitReader(body, size+1))
				err = errors.Join(err, body.Close())
				if err != nil {
					return err
				}
				if n != size {
					return assets.ErrInvalidUpload
				}
			}
			return nil
		}()
		if err := errors.Join(copyErr, f.Close()); err != nil {
			return err
		}
		end := math.Min(float64(i+1)*30, r.DurationSeconds)
		// Validation permits a frame of declared-duration rounding error.
		// Count available sample instants from real packet PTS so a tail just
		// beyond a five-second boundary cannot expose tile's black padding.
		packetData, err := ProbeCommand(ctx, p.FFprobe, "-v", "error", "-protocol_whitelist", "file", "-enable_drefs", "0", "-use_absolute_path", "0", "-select_streams", "v:0", "-show_packets", "-show_entries", "packet=pts_time", "-of", "json", input)
		if err != nil {
			return err
		}
		cells, err := previewCells(packetData, end-float64(i)*30)
		if err != nil {
			return err
		}
		filter := fmt.Sprintf("setpts=PTS-STARTPTS,select='gte(t,selected_n*5)',scale=160:90:force_original_aspect_ratio=decrease,pad=160:90:(ow-iw)/2:(oh-ih)/2,tile=6x1:nb_frames=%d", cells)
		decodeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err = exec.CommandContext(decodeCtx, p.FFmpeg, "-nostdin", "-v", "error", "-xerror", "-protocol_whitelist", "file", "-enable_drefs", "0", "-use_absolute_path", "0", "-threads", "2", "-i", input, "-an", "-vf", filter, "-frames:v", "1", "-threads", "1", "-q:v", "4", "-fs", "1048576", output).Run()
		cancel()
		if err != nil {
			return err
		}
		imageFile, err := os.Open(output)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(imageFile, (1<<20)+1))
		err = errors.Join(err, imageFile.Close())
		if err != nil {
			return err
		}
		if len(data) > 1<<20 {
			return assets.ErrInvalidUpload
		}
		image, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil || image.Bounds().Dx() != 960 || image.Bounds().Dy() != 90 {
			return assets.ErrInvalidUpload
		}
		if err := put(ctx, outPrefix+fmt.Sprintf("seg-%06d.jpg", i), data); err != nil {
			return err
		}
		for cell := 0; cell < cells; cell++ {
			start := float64(i*30 + cell*5)
			fmt.Fprintf(&vtt, "%s --> %s\nseg-%06d.jpg#xywh=%d,0,160,90\n\n", previewTimestamp(start), previewTimestamp(math.Min(start+5, end)), i, cell*160)
		}
		if vtt.Len() > 1<<20 {
			return assets.ErrInvalidUpload
		}
		if err := errors.Join(os.Remove(input), os.Remove(output)); err != nil {
			return err
		}
	}
	return put(ctx, outPrefix+"index.vtt", []byte(vtt.String()))
}

func previewCells(data []byte, duration float64) (int, error) {
	var output struct {
		Packets []struct {
			PTS string `json:"pts_time"`
		} `json:"packets"`
	}
	if json.Unmarshal(data, &output) != nil || len(output.Packets) == 0 || len(output.Packets) > 4096 {
		return 0, assets.ErrInvalidUpload
	}
	first, last := math.Inf(1), math.Inf(-1)
	for _, packet := range output.Packets {
		pts, err := strconv.ParseFloat(packet.PTS, 64)
		if err != nil || math.IsNaN(pts) || math.IsInf(pts, 0) {
			return 0, assets.ErrInvalidUpload
		}
		first = math.Min(first, pts)
		last = math.Max(last, pts)
	}
	cells := min(6, int(math.Ceil(duration/5)), int(math.Floor((last-first+0.000001)/5))+1)
	if cells < 1 {
		return 0, assets.ErrInvalidUpload
	}
	return cells, nil
}

func previewTimestamp(seconds float64) string {
	ms := int64(math.Round(seconds * 1000))
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}
