package recordingvalidation

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"

	"hhc/asset-api/internal/assets"
)

// A successful conditional copy of a receipt-bound version preserves the decoded bytes.
// The attempt prefix and ready commit remain owned by the existing package lease.
func freezeLiveArchive(ctx context.Context, p assets.RecordingPackage, attempt string, history []assets.RecordingLiveSegment, objects PackageObjects, probe PackageMediaProbe) (string, bool, error) {
	if !packageIDPattern.MatchString(p.ID) || !packageIDPattern.MatchString(attempt) || objects == nil {
		return "", false, assets.ErrInvalidInput
	}
	size, err := assets.ValidateRecordingInventory(p.Inventory)
	if err != nil || size != p.SizeBytes {
		return "", false, assets.ErrInvalidUpload
	}
	versions := make(map[string]string)
	for _, segment := range history {
		for _, fragment := range segment.Renditions {
			receipt := fragment.Validation
			if receipt == nil || receipt.Version != liveValidationVersion || receipt.InitETag == "" || receipt.SegmentETag == "" {
				return "", false, nil
			}
			for path, version := range map[string]string{receipt.Init.Path: receipt.InitETag, receipt.Segment.Path: receipt.SegmentETag} {
				if strings.ContainsAny(version, "\r\n") {
					return "", false, nil
				}
				if prior, ok := versions[path]; ok && prior != version {
					return "", false, nil
				}
				versions[path] = version
			}
		}
	}
	if len(history) == 0 {
		return "", false, nil
	}
	prefix := "recordings/packages/" + p.ID + "/final/" + attempt + "/"
	playlists := make(map[string][]byte)
	for _, object := range p.Inventory.Objects {
		if !strings.HasSuffix(object.Path, ".m3u8") {
			continue
		}
		var data bytes.Buffer
		if err := freezePackageObject(ctx, p, prefix, object, objects, &data, probe.OnStage); err != nil {
			return "", true, err
		}
		playlists[object.Path] = data.Bytes()
	}
	if err := assets.ValidateRecordingPlaylists(p.Inventory, playlists); err != nil {
		return "", true, err
	}
	if err := validateLiveArchiveEvidence(p.Inventory, playlists["master.m3u8"], history); err != nil {
		return "", false, nil
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan assets.RecordingPackageObject, len(p.Inventory.Objects))
	for _, object := range p.Inventory.Objects {
		if !strings.HasSuffix(object.Path, ".m3u8") {
			jobs <- object
		}
	}
	close(jobs)
	var workers sync.WaitGroup
	var mu sync.Mutex
	var failure error
	// Bound provider work independently of media decoders; no media body is downloaded.
	for worker := 0; worker < 16; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for object := range jobs {
				if workCtx.Err() != nil {
					return
				}
				err := objects.CopyPackageObject(workCtx, "recordings/captures/"+p.ID+"/final/"+object.Path, prefix+object.Path, versions[object.Path])
				if err != nil {
					mu.Lock()
					if failure == nil {
						failure = err
					}
					mu.Unlock()
					cancel()
					return
				}
			}
		}()
	}
	workers.Wait()
	if failure != nil {
		return "", true, failure
	}
	if err := ctx.Err(); err != nil {
		return "", true, err
	}
	inventory, err := json.Marshal(p.Inventory)
	if err != nil {
		return "", true, err
	}
	if err := objects.PutPackageInventory(ctx, prefix+"package.json", inventory); err != nil {
		return "", true, err
	}
	return prefix, true, nil
}
