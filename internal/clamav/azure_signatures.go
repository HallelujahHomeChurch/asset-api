package clamav

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
)

const maxSignatureBlobSize = 512 << 20

type AzureSignatures struct {
	download func(context.Context, string) ([]byte, error)
	upload   func(context.Context, string, string, bool) error
}

func NewAzureSignatures(accountURL, container string) (*AzureSignatures, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	client, err := azblob.NewClient(accountURL, credential, nil)
	if err != nil {
		return nil, err
	}
	store := &AzureSignatures{}
	store.download = func(ctx context.Context, name string) ([]byte, error) {
		response, err := client.DownloadStream(ctx, container, name, nil)
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return nil, os.ErrNotExist
		}
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, maxSignatureBlobSize+1))
		if err == nil && len(data) > maxSignatureBlobSize {
			err = errors.New("signature blob exceeds size limit")
		}
		return data, err
	}
	store.upload = func(ctx context.Context, name, path string, immutable bool) error {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		options := &azblob.UploadFileOptions{HTTPHeaders: &blob.HTTPHeaders{BlobContentType: ptr("application/octet-stream")}}
		if immutable {
			any := azcore.ETagAny
			options.AccessConditions = &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &any}}
		}
		_, err = client.UploadFile(ctx, container, name, file, options)
		return err
	}
	return store, nil
}

func (s *AzureSignatures) PrepareCurrent(ctx context.Context, now time.Time, maxAge time.Duration) (Manifest, string, func(), error) {
	data, err := s.download(ctx, "current.json")
	if err != nil {
		return Manifest{}, "", nil, fmt.Errorf("download current signature manifest: %w", err)
	}
	root, err := os.MkdirTemp("", "clamav-signatures-")
	if err != nil {
		return Manifest{}, "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	manifestPath := filepath.Join(root, "manifest.json")
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		cleanup()
		return Manifest{}, "", nil, err
	}
	manifest, database, err := LoadManifest(manifestPath, now, maxAge)
	if err != nil {
		cleanup()
		return Manifest{}, "", nil, err
	}
	if err := os.MkdirAll(database, 0o700); err != nil {
		cleanup()
		return Manifest{}, "", nil, err
	}
	for _, file := range manifest.Files {
		data, err := s.download(ctx, manifest.DatabaseDirectory+"/"+file)
		if err != nil {
			cleanup()
			return Manifest{}, "", nil, fmt.Errorf("download signature %s: %w", file, err)
		}
		if err := os.WriteFile(filepath.Join(database, file), data, 0o600); err != nil {
			cleanup()
			return Manifest{}, "", nil, err
		}
	}
	return manifest, database, cleanup, nil
}

func (s *AzureSignatures) Publish(ctx context.Context, localRoot string, manifest Manifest) (Manifest, error) {
	if current, err := s.download(ctx, "current.json"); err == nil {
		var previous Manifest
		if json.Unmarshal(current, &previous) == nil && signatureName.MatchString(previous.SignatureVersion) {
			manifest.PreviousSignatureVersion = previous.SignatureVersion
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	for _, file := range manifest.Files {
		local := filepath.Join(localRoot, "current", filepath.FromSlash(manifest.DatabaseDirectory), file)
		if err := s.upload(ctx, manifest.DatabaseDirectory+"/"+file, local, true); err != nil {
			return Manifest{}, err
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return Manifest{}, err
	}
	temporary := filepath.Join(localRoot, "current.json")
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return Manifest{}, err
	}
	if err := s.upload(ctx, "current.json", temporary, false); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func ptr[T any](value T) *T { return &value }
