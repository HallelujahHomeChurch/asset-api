package recordingvalidation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/storage/r2"
)

var packageIDPattern = regexp.MustCompile(`^[a-zA-Z0-9-]{1,80}$`)

type PackageObjects interface {
	Head(context.Context, string) (int64, string, error)
	CopyPackageObject(context.Context, string, string, string) error
	Open(context.Context, string) (io.ReadCloser, error)
	PutPackageInventory(context.Context, string, []byte) error
}

// FreezeRecordingPackage never probes mutable staging. Each attempt has its
// own server-only prefix; callers must fence the eventual ready DB commit.
// Only playlists are held in memory, never the complete package or source.
func FreezeRecordingPackage(ctx context.Context, p assets.RecordingPackage, attempt string, objects PackageObjects, probe PackageMediaProbe) (prefix string, result error) {
	stage := "inventory"
	stageStarted := time.Now()
	reportStage := func() {
		if probe.OnStage != nil {
			probe.OnStage(stage, time.Since(stageStarted))
		}
	}
	nextStage := func(next string) { reportStage(); stage, stageStarted = next, time.Now() }
	defer func() { reportStage(); result = packageStageError(stage, result) }()
	if !packageIDPattern.MatchString(p.ID) || !packageIDPattern.MatchString(attempt) || objects == nil || !filepath.IsAbs(probe.FFmpeg) || !filepath.IsAbs(probe.FFprobe) || p.Inventory.InventoryDigest == "" {
		return "", assets.ErrInvalidInput
	}
	size, err := assets.ValidateRecordingInventory(p.Inventory)
	if err != nil {
		return "", err
	}
	if size != p.SizeBytes {
		return "", assets.ErrInvalidInput
	}
	prefix = "recordings/packages/" + p.ID + "/final/" + attempt + "/"
	freezeObject := func(ctx context.Context, object assets.RecordingPackageObject, destination io.Writer) error {
		started := time.Now()
		defer func() {
			if probe.OnStage != nil {
				probe.OnStage("freeze_hash", time.Since(started))
			}
		}()
		return freezePackageObject(ctx, p, prefix, object, objects, destination, probe.OnStage)
	}
	nextStage("playlists")
	playlists := map[string][]byte{}
	inventoryObjects := make(map[string]assets.RecordingPackageObject, len(p.Inventory.Objects))
	for _, object := range p.Inventory.Objects {
		inventoryObjects[object.Path] = object
		if strings.HasSuffix(object.Path, ".m3u8") {
			var manifest bytes.Buffer
			if err := freezeObject(ctx, object, &manifest); err != nil {
				return "", err
			}
			playlists[object.Path] = manifest.Bytes()
		}
	}
	if err := assets.ValidateRecordingPlaylists(p.Inventory, playlists); err != nil {
		return "", err
	}
	freeze := func(ctx context.Context, path string, w io.Writer) error {
		object, ok := inventoryObjects[path]
		if !ok {
			return assets.ErrInvalidUpload
		}
		return freezeObject(ctx, object, w)
	}
	nextStage("media")
	if err := probe.validateMedia(ctx, p.Inventory, playlists["master.m3u8"], freeze, freeze); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	nextStage("inventory_write")
	inventory, err := json.Marshal(p.Inventory)
	if err != nil {
		return "", err
	}
	if err := objects.PutPackageInventory(ctx, prefix+"package.json", inventory); err != nil {
		return "", err
	}
	return prefix, nil
}

// The final stream is hashed while writing its owned scratch destination.
// Init bytes prepended by the caller are never included in the fragment hash.
func freezePackageObject(ctx context.Context, p assets.RecordingPackage, prefix string, object assets.RecordingPackageObject, objects PackageObjects, destination io.Writer, onStage func(string, time.Duration)) (result error) {
	stage := "object_head_staging"
	timingStage, started := stage, time.Now()
	reportStage := func() {
		if onStage != nil {
			onStage(timingStage, time.Since(started))
		}
	}
	nextTimingStage := func(next string) { reportStage(); timingStage, started = next, time.Now() }
	defer func() { reportStage(); result = packageStageError(stage, result) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	size, etag, err := objects.Head(ctx, p.StagingKey(object.Path))
	if errors.Is(err, r2.ErrNotFound) || errors.Is(err, assets.ErrNotFound) {
		return assets.ErrInvalidUpload
	}
	if err != nil {
		return err
	}
	if size != object.SizeBytes || etag == "" {
		return assets.ErrInvalidUpload
	}
	key := prefix + object.Path
	nextTimingStage("object_copy")
	stage = "object_copy"
	if err := objects.CopyPackageObject(ctx, p.StagingKey(object.Path), key, etag); err != nil {
		return err
	}
	nextTimingStage("object_head_final")
	stage = "object_head_final"
	size, _, err = objects.Head(ctx, key)
	if err != nil {
		return err
	}
	if size != object.SizeBytes {
		return assets.ErrInvalidUpload
	}
	nextTimingStage("object_download_hash")
	stage = "object_open"
	body, err := objects.Open(ctx, key)
	if err != nil {
		return err
	}
	stage = "object_read"
	hash := sha256.New()
	n, readErr := io.Copy(io.MultiWriter(hash, destination), io.LimitReader(body, object.SizeBytes+1))
	if err := errors.Join(readErr, body.Close()); err != nil {
		return err
	}
	stage = "object_hash"
	if n != object.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != object.SHA256 {
		return assets.ErrInvalidUpload
	}
	return nil
}
