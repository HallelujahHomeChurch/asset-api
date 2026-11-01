package clamav

import (
	"context"
	"errors"
	"reflect"
	"time"
)

// SignatureCache belongs to one serial scanner loop; each scan finishes before Prepare runs again.
type SignatureCache struct {
	store     *AzureSignatures
	manifest  Manifest
	directory string
	cleanup   func()
}

func NewSignatureCache(store *AzureSignatures) *SignatureCache {
	return &SignatureCache{store: store}
}

func (c *SignatureCache) Prepare(ctx context.Context, now time.Time, maxAge time.Duration) (Manifest, string, error) {
	manifest, err := c.store.currentManifest(ctx, now, maxAge)
	if err != nil {
		return Manifest{}, "", err
	}
	if c.directory != "" && manifest.SignatureVersion == c.manifest.SignatureVersion {
		if !reflect.DeepEqual(manifest, c.manifest) {
			return Manifest{}, "", errors.New("immutable signature manifest changed")
		}
		return manifest, c.directory, nil
	}
	directory, cleanup, err := c.store.prepare(ctx, manifest)
	if err != nil {
		return Manifest{}, "", err
	}
	previous := c.cleanup
	c.manifest, c.directory, c.cleanup = manifest, directory, cleanup
	if previous != nil {
		previous()
	}
	return manifest, directory, nil
}

func (c *SignatureCache) Close() {
	if c.cleanup != nil {
		c.cleanup()
	}
	c.manifest, c.directory, c.cleanup = Manifest{}, "", nil
}
