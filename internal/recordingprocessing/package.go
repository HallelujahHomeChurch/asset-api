package recordingprocessing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/recordingvalidation"
)

// BuildSourcePackage measures the completed output, not the encoder's target
// bitrate. It only uploads control files after validating the complete graph;
// callers must still freeze, hash and decode every fragment before marking ready.
func BuildSourcePackage(ctx context.Context, plan SourcePlan, spool *OutputSpool, probe recordingvalidation.PackageMediaProbe) (assets.RecordingPackageInventory, error) {
	media, lists, err := spool.Snapshot()
	if err != nil {
		return assets.RecordingPackageInventory{}, err
	}
	inv := assets.RecordingPackageInventory{SchemaVersion: 1, PresetVersion: "hls-v1", Objects: media, Renditions: slices.Clone(plan.Renditions)}
	invalid := func(err error) (assets.RecordingPackageInventory, error) {
		return assets.RecordingPackageInventory{}, err
	}
	if len(inv.Renditions) < 1 || len(inv.Renditions) > 3 || len(lists) != len(inv.Renditions) {
		return invalid(assets.ErrInvalidUpload)
	}
	sizes := make(map[string]int64, len(media))
	for _, object := range media {
		sizes[object.Path] = object.SizeBytes
	}
	master := new(strings.Builder)
	master.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	pkg := assets.RecordingPackage{ID: spool.packageID}
	for i := range inv.Renditions {
		r := &inv.Renditions[i]
		var durations []float64
		var segmentSizes []int64
		target, duration := 0, 0.0
		for _, line := range strings.Split(string(lists[r.Name+"/index.m3u8"]), "\n") {
			if value, ok := strings.CutPrefix(line, "#EXT-X-TARGETDURATION:"); ok {
				target, err = strconv.Atoi(value)
				if err != nil {
					return invalid(assets.ErrInvalidUpload)
				}
			}
			if value, ok := strings.CutPrefix(line, "#EXTINF:"); ok {
				d, err := strconv.ParseFloat(strings.TrimSuffix(value, ","), 64)
				if err != nil || !positiveFinite(d) {
					return invalid(assets.ErrInvalidUpload)
				}
				segmentSizes = append(segmentSizes, sizes[fmt.Sprintf("%s/seg-%06d.m4s", r.Name, len(durations))])
				durations = append(durations, d)
				duration += d
			}
		}
		if !positiveFinite(r.FrameRate) || math.Abs(duration-r.DurationSeconds) > 1/r.FrameRate+0.001 {
			return invalid(assets.ErrInvalidUpload)
		}
		r.DurationSeconds, r.SegmentCount = duration, len(durations)
		peak, average, err := assets.RecordingPlaylistBitrates(segmentSizes, durations, target)
		if err != nil {
			return invalid(err)
		}
		codecs, err := probe.InitCodecs(ctx, pkg.StagingKey(""), r.Name, sizes[r.Name+"/init.mp4"])
		if err != nil {
			return invalid(fmt.Errorf("init codecs: %w", err))
		}
		fmt.Fprintf(master, "#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,RESOLUTION=%dx%d,FRAME-RATE=%.3f,CODECS=%q\n%s/index.m3u8\n", peak, average, r.Width, r.Height, r.FrameRate, codecs, r.Name)
	}
	lists["master.m3u8"] = []byte(master.String())
	for path, data := range lists {
		hash := sha256.Sum256(data)
		inv.Objects = append(inv.Objects, assets.RecordingPackageObject{Path: path, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(hash[:])})
	}
	slices.SortFunc(inv.Objects, func(a, b assets.RecordingPackageObject) int { return strings.Compare(a.Path, b.Path) })
	inv.InventoryDigest, err = assets.RecordingInventoryDigest(inv)
	if err != nil {
		return invalid(err)
	}
	if err := assets.ValidateRecordingPlaylists(inv, lists); err != nil {
		return invalid(fmt.Errorf("output playlists: %w", err))
	}
	for _, object := range inv.Objects {
		data, ok := lists[object.Path]
		if !ok {
			continue
		}
		if err := spool.objects.PutRecordingObject(ctx, pkg.StagingKey(object.Path), bytes.NewReader(data), int64(len(data)), "application/vnd.apple.mpegurl"); err != nil {
			return invalid(ErrOutputUpload)
		}
	}
	return inv, nil
}
