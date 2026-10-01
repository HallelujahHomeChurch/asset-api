package azure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"hhc/asset-api/internal/assets"
)

var recordingSourceID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// DeleteRecordingSource cannot accept an arbitrary Blob key or container.
// The repository supplies all <=3 durable attempt IDs, including failed copies.
func (s *Store) DeleteRecordingSource(ctx context.Context, id string, attempts []string) error {
	staging, err := recordingSourceKey(id)
	if err != nil || len(attempts) > 3 {
		return assets.ErrInvalidInput
	}
	keys := []string{staging}
	seen := map[string]bool{}
	for _, attempt := range attempts {
		if !recordingSourceID.MatchString(attempt) || seen[attempt] {
			return assets.ErrInvalidInput
		}
		seen[attempt] = true
		keys = append(keys, "recording-sources/"+id+"/final/"+attempt+"/source")
	}
	for _, key := range keys {
		if err := s.Delete(ctx, key); err != nil {
			return recordingSourceError(err)
		}
	}
	return nil
}

func recordingSourceKey(id string) (string, error) {
	if !recordingSourceID.MatchString(id) {
		return "", assets.ErrInvalidInput
	}
	return "recording-sources/" + id + "/staging", nil
}

// CopyRecordingSource is a restartable metadata operation. The same-account
// copy is authorized by the Store's managed identity, without issuing a read SAS.
// Callers hold a fenced job lease and retain the attempt ID before calling it.
// A successful provider copy still requires a streaming SHA-256 verification.
func (s *Store) CopyRecordingSource(ctx context.Context, source assets.RecordingSource, attempt string) (assets.RecordingSourceCopy, error) {
	staging, err := recordingSourceKey(source.ID)
	if err != nil || !recordingSourceID.MatchString(attempt) || source.StagingETag == "" || assets.ValidateRecordingSourceSize(source.SizeBytes) != nil || len(source.ChecksumSHA256) != 64 {
		return assets.RecordingSourceCopy{}, assets.ErrInvalidInput
	}
	if _, err := hex.DecodeString(source.ChecksumSHA256); err != nil {
		return assets.RecordingSourceCopy{}, assets.ErrInvalidInput
	}
	key := "recording-sources/" + source.ID + "/final/" + attempt + "/source"
	container := s.client.ServiceClient().NewContainerClient(s.container)
	destination := container.NewBlobClient(key)
	sourceURL := container.NewBlobClient(staging).URL()
	u, err := url.Parse(sourceURL)
	if err != nil || u.Scheme != "https" || u.RawQuery != "" {
		return assets.RecordingSourceCopy{}, assets.ErrInvalidInput
	}
	digest := sha256.Sum256([]byte(source.StagingETag))
	etagHash := hex.EncodeToString(digest[:])
	metadata := map[string]*string{"hhc_source": &source.ID, "hhc_etag": &etagHash, "hhc_sha256": &source.ChecksumSHA256}
	properties, err := destination.GetProperties(ctx, nil)
	if errors.Is(mapError(err), assets.ErrNotFound) {
		match, absent := azcore.ETag(source.StagingETag), azcore.ETagAny
		_, err = destination.StartCopyFromURL(ctx, sourceURL, &blob.StartCopyFromURLOptions{
			SourceModifiedAccessConditions: &blob.SourceModifiedAccessConditions{SourceIfMatch: &match},
			AccessConditions:               &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &absent}}, Metadata: metadata,
		})
		// A lost copy-start response or concurrent recovery may have created the
		// destination. Re-read it, but never overwrite an existing object.
		if err != nil {
			var response *azcore.ResponseError
			if !errors.As(err, &response) || response.StatusCode != http.StatusPreconditionFailed {
				return assets.RecordingSourceCopy{}, recordingSourceError(err)
			}
		}
		properties, err = destination.GetProperties(ctx, nil)
	}
	if err != nil {
		return assets.RecordingSourceCopy{}, recordingSourceError(err)
	}
	for name, want := range metadata {
		found := false
		for key, got := range properties.Metadata {
			if strings.EqualFold(key, name) {
				if got == nil || *got != *want {
					return assets.RecordingSourceCopy{}, assets.ErrInvalidUpload
				}
				found = true
			}
		}
		if !found {
			return assets.RecordingSourceCopy{}, assets.ErrInvalidUpload
		}
	}
	if properties.CopyID == nil || *properties.CopyID == "" || properties.CopyStatus == nil {
		return assets.RecordingSourceCopy{}, assets.ErrInvalidUpload
	}
	result := assets.RecordingSourceCopy{Key: key, CopyID: *properties.CopyID, State: string(*properties.CopyStatus)}
	switch result.State {
	case "pending", "failed", "aborted":
		return result, nil
	case "success":
		if properties.ContentLength == nil || *properties.ContentLength != source.SizeBytes || properties.ETag == nil || *properties.ETag == "" {
			return assets.RecordingSourceCopy{}, assets.ErrInvalidUpload
		}
		result.SizeBytes, result.ETag = *properties.ContentLength, string(*properties.ETag)
		return result, nil
	default:
		return assets.RecordingSourceCopy{}, assets.ErrInvalidUpload
	}
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
