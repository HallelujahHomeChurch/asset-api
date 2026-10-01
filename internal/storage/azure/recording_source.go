package azure

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"hhc/asset-api/internal/assets"
)

var recordingSourceID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func recordingSourceKey(id string) (string, error) {
	if !recordingSourceID.MatchString(id) {
		return "", assets.ErrInvalidInput
	}
	return "recording-sources/" + id + "/staging", nil
}

// A blob-scoped write SAS also permits other writes to this staging blob. It is
// not a byte quota or an immutable-upload grant. Finalization must pin its ETag
// and copy/hash into a server-only source before any media processing.
func (s *Store) SignRecordingSourceBlock(ctx context.Context, id string, number int, expires time.Time) (assets.UploadTarget, error) {
	key, err := recordingSourceKey(id)
	if err != nil {
		return assets.UploadTarget{}, err
	}
	blockID, err := assets.RecordingSourceBlockID(number)
	if err != nil {
		return assets.UploadTarget{}, err
	}
	now := time.Now().UTC()
	if !expires.After(now) || expires.After(now.Add(assets.RecordingPartURLTTL)) {
		return assets.UploadTarget{}, assets.ErrInvalidInput
	}
	u, err := url.Parse(s.client.ServiceClient().NewContainerClient(s.container).NewBlobClient(key).URL())
	if err != nil || u.Scheme != "https" {
		return assets.UploadTarget{}, assets.ErrInvalidInput
	}
	credential, err := s.userDelegationCredential(ctx)
	if err != nil {
		return assets.UploadTarget{}, recordingSourceError(err)
	}
	query, err := (sas.BlobSignatureValues{Protocol: sas.ProtocolHTTPS, StartTime: now.Add(-5 * time.Minute), ExpiryTime: expires.UTC(), Permissions: (&sas.BlobPermissions{Write: true}).String(), ContainerName: s.container, BlobName: key}).SignWithUserDelegation(credential)
	if err != nil {
		return assets.UploadTarget{}, recordingSourceError(err)
	}
	values, err := url.ParseQuery(query.Encode())
	if err != nil {
		return assets.UploadTarget{}, assets.ErrRecordingStorageUnavailable
	}
	values.Set("comp", "block")
	values.Set("blockid", blockID)
	u.RawQuery = values.Encode()
	return assets.UploadTarget{URL: u.String(), Method: http.MethodPut, Headers: map[string]string{"Content-Type": "application/octet-stream"}, ExpiresAt: expires.UTC()}, nil
}

func (s *Store) RecordingSourceBlocks(ctx context.Context, id string) (assets.RecordingSourceBlockList, error) {
	key, err := recordingSourceKey(id)
	if err != nil {
		return assets.RecordingSourceBlockList{}, err
	}
	response, err := s.client.ServiceClient().NewContainerClient(s.container).NewBlockBlobClient(key).GetBlockList(ctx, blockblob.BlockListTypeAll, nil)
	if err != nil {
		return assets.RecordingSourceBlockList{}, recordingSourceError(err)
	}
	if len(response.CommittedBlocks) > assets.RecordingSourceMaxBlocks || len(response.UncommittedBlocks) > assets.RecordingSourceMaxBlocks {
		return assets.RecordingSourceBlockList{}, assets.ErrInvalidUpload
	}
	var value assets.RecordingSourceBlockList
	if response.ETag != nil {
		value.ETag = string(*response.ETag)
	}
	for _, block := range response.CommittedBlocks {
		if block == nil || block.Name == nil || block.Size == nil || len(*block.Name) != 12 || *block.Size <= 0 || *block.Size > assets.RecordingSourceBlockBytes {
			return value, assets.ErrInvalidUpload
		}
		value.Committed = append(value.Committed, assets.RecordingSourceBlock{ID: *block.Name, SizeBytes: *block.Size})
	}
	for _, block := range response.UncommittedBlocks {
		if block == nil || block.Name == nil || block.Size == nil || len(*block.Name) != 12 || *block.Size <= 0 || *block.Size > assets.RecordingSourceBlockBytes {
			return value, assets.ErrInvalidUpload
		}
		value.Uncommitted = append(value.Uncommitted, assets.RecordingSourceBlock{ID: *block.Name, SizeBytes: *block.Size})
	}
	return value, nil
}

// CommitRecordingSource only closes the declared block list. No full-object
// read/hash/copy occurs in the API request, and this never establishes ready.
func (s *Store) CommitRecordingSource(ctx context.Context, id string, size int64) (assets.BlobMetadata, error) {
	if err := assets.ValidateRecordingSourceSize(size); err != nil {
		return assets.BlobMetadata{}, err
	}
	key, err := recordingSourceKey(id)
	if err != nil {
		return assets.BlobMetadata{}, err
	}
	blocks, err := s.RecordingSourceBlocks(ctx, id)
	if err != nil {
		return assets.BlobMetadata{}, err
	}
	ids, err := assets.ValidateRecordingSourceBlocks(size, blocks.Committed, blocks.Uncommitted)
	if err != nil {
		return assets.BlobMetadata{}, err
	}
	conditions := &blob.ModifiedAccessConditions{}
	if blocks.ETag != "" {
		etag := azcore.ETag(blocks.ETag)
		conditions.IfMatch = &etag
	} else {
		any := azcore.ETagAny
		conditions.IfNoneMatch = &any
	}
	contentType := "application/octet-stream"
	response, err := s.client.ServiceClient().NewContainerClient(s.container).NewBlockBlobClient(key).CommitBlockList(ctx, ids, &blockblob.CommitBlockListOptions{AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: conditions}, HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &contentType}})
	if err != nil {
		return assets.BlobMetadata{}, recordingSourceError(err)
	}
	if response.ETag == nil || *response.ETag == "" {
		return assets.BlobMetadata{}, assets.ErrInvalidUpload
	}
	metadata, err := s.InspectProperties(ctx, key)
	if err != nil {
		return assets.BlobMetadata{}, recordingSourceError(err)
	}
	if metadata.Size != size || metadata.ETag != string(*response.ETag) {
		return assets.BlobMetadata{}, assets.ErrInvalidUpload
	}
	return metadata, nil
}

func recordingSourceError(err error) error {
	err = mapError(err)
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, assets.ErrNotFound, assets.ErrConflict, assets.ErrInvalidUpload, assets.ErrInvalidInput} {
		if errors.Is(err, known) {
			return known
		}
	}
	return assets.ErrRecordingStorageUnavailable // Do not forward SDK URLs/credentials into logs.
}
