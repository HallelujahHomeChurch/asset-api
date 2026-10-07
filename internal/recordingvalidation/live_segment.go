package recordingvalidation

import (
	"context"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"io"
	"os"
	"path/filepath"
)

// ValidateLiveSegment decodes only the next three closed fragments. Copies are
// attempt-private; the caller publishes them only after the whole batch passes.
func (p PackageMediaProbe) ValidateLiveSegment(ctx context.Context, captureID, attempt string, sequence int, declarations []assets.RecordingPackageObject) (result map[string]assets.RecordingLiveFragment, err error) {
	if !packageIDPattern.MatchString(captureID) || !packageIDPattern.MatchString(attempt) || sequence < 0 || sequence >= 1440 || p.Objects == nil || !filepath.IsAbs(p.FFmpeg) || !filepath.IsAbs(p.FFprobe) {
		return nil, assets.ErrInvalidInput
	}
	selected := map[string]assets.RecordingPackageObject{}
	for _, o := range declarations {
		if _, exists := selected[o.Path]; exists {
			return nil, assets.ErrInvalidInput
		}
		selected[o.Path] = o
	}
	for _, r := range assets.LiveRenditions() {
		for _, name := range []string{"init.mp4", fmt.Sprintf("seg-%06d.m4s", sequence)} {
			o, ok := selected[r.Name+"/"+name]
			if !ok {
				return nil, assets.ErrCaptureMissingObjects
			}
			if o.SizeBytes < 1 || o.SizeBytes > assets.RecordingObjectMaxBytes || len(o.SHA256) != 64 {
				return nil, assets.ErrInvalidInput
			}
		}
	}
	dir, err := os.MkdirTemp(p.ScratchRoot, "hhc-live-fragment-")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	pack := assets.RecordingPackage{ID: captureID}
	prefix := "recordings/packages/" + captureID + "/final/" + attempt + "/"
	result = make(map[string]assets.RecordingLiveFragment, 3)
	for _, r := range assets.LiveRenditions() {
		init := selected[r.Name+"/init.mp4"]
		segment := selected[fmt.Sprintf("%s/seg-%06d.m4s", r.Name, sequence)]
		if err := checkFragmentScratch(dir, init.SizeBytes+segment.SizeBytes); err != nil {
			return nil, err
		}
		path := filepath.Join(dir, r.Name+".mp4")
		if err := writePackageScratch(path, func(w io.Writer) error {
			if err := freezePackageObject(ctx, pack, prefix, init, p.Objects, w); err != nil {
				return err
			}
			return freezePackageObject(ctx, pack, prefix, segment, p.Objects, w)
		}); err != nil {
			return nil, err
		}
		actual, err := p.probeFragment(ctx, path, r)
		if err != nil {
			return nil, err
		}
		if actual.AudioChannels != 2 {
			return nil, assets.ErrInvalidUpload
		}
		result[r.Name] = assets.RecordingLiveFragment{Start: actual.Start, End: actual.End, Codecs: actual.Codecs}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	return result, nil
}
