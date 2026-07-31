package azure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"hhc/asset-api/internal/assets"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
)

type Store struct {
	client            *azblob.Client
	container         string
	mu                sync.Mutex
	delegation        *service.UserDelegationCredential
	delegationExpires time.Time
}

func New(accountURL, container string) (*Store, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	client, err := azblob.NewClient(strings.TrimRight(accountURL, "/"), credential, nil)
	if err != nil {
		return nil, err
	}
	return &Store{client: client, container: container}, nil
}

func (s *Store) CreateUploadTarget(ctx context.Context, objectKey string, _ int64, expiresAt time.Time) (assets.UploadTarget, error) {
	credential, err := s.userDelegationCredential(ctx)
	if err != nil {
		return assets.UploadTarget{}, err
	}
	query, err := (sas.BlobSignatureValues{Protocol: sas.ProtocolHTTPS, StartTime: time.Now().UTC().Add(-5 * time.Minute), ExpiryTime: expiresAt.UTC(), Permissions: (&sas.BlobPermissions{Create: true}).String(), ContainerName: s.container, BlobName: objectKey}).SignWithUserDelegation(credential)
	if err != nil {
		return assets.UploadTarget{}, err
	}
	blobURL := s.client.ServiceClient().NewContainerClient(s.container).NewBlobClient(objectKey).URL()
	return assets.UploadTarget{URL: blobURL + "?" + query.Encode(), Method: http.MethodPut, Headers: map[string]string{"x-ms-blob-type": "BlockBlob", "Content-Type": "application/octet-stream"}, ExpiresAt: expiresAt}, nil
}

func (s *Store) InspectProperties(ctx context.Context, objectKey string) (assets.BlobMetadata, error) {
	response, err := s.client.ServiceClient().NewContainerClient(s.container).NewBlobClient(objectKey).GetProperties(ctx, nil)
	if err != nil {
		return assets.BlobMetadata{}, mapError(err)
	}
	metadata := assets.BlobMetadata{}
	if response.ContentLength != nil {
		metadata.Size = *response.ContentLength
	}
	if response.ContentType != nil {
		metadata.ContentType = *response.ContentType
	}
	if response.ETag != nil {
		metadata.ETag = string(*response.ETag)
	}
	if response.LastModified != nil {
		metadata.LastModified = *response.LastModified
	}
	return metadata, nil
}

func (s *Store) Inspect(ctx context.Context, objectKey, expectedETag string, maxSize int64) (assets.BlobProperties, error) {
	options := &azblob.DownloadStreamOptions{}
	if expectedETag != "" {
		match := azcore.ETag(expectedETag)
		options.AccessConditions = &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &match}}
	}
	response, err := s.client.DownloadStream(ctx, s.container, objectKey, options)
	if err != nil {
		return assets.BlobProperties{}, mapError(err)
	}
	defer response.Body.Close()
	hash := sha256.New()
	header := make([]byte, 512)
	read, readErr := io.ReadFull(response.Body, header)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return assets.BlobProperties{}, readErr
	}
	if _, err := hash.Write(header[:read]); err != nil {
		return assets.BlobProperties{}, err
	}
	size := int64(read)
	reader := io.Reader(response.Body)
	if maxSize > 0 {
		reader = io.LimitReader(response.Body, maxSize-int64(read)+1)
	}
	written, err := io.Copy(hash, reader)
	if err != nil {
		return assets.BlobProperties{}, err
	}
	size += written
	if maxSize > 0 && size > maxSize {
		return assets.BlobProperties{}, assets.ErrInvalidUpload
	}
	mime := http.DetectContentType(header[:read])
	if strings.HasPrefix(string(header[:read]), "%PDF-") {
		mime = "application/pdf"
	}
	etag := ""
	if response.ETag != nil {
		etag = string(*response.ETag)
	}
	return assets.BlobProperties{Size: size, DetectedMIMEType: mime, ChecksumSHA256: hex.EncodeToString(hash.Sum(nil)), ETag: etag}, nil
}

func (s *Store) Commit(ctx context.Context, stagingObjectKey, finalObjectKey string) (assets.BlobProperties, error) {
	response, err := s.client.DownloadStream(ctx, s.container, stagingObjectKey, nil)
	if err != nil {
		if properties, inspectErr := s.Inspect(ctx, finalObjectKey, "", 0); inspectErr == nil {
			return properties, nil
		}
		return assets.BlobProperties{}, mapError(err)
	}
	defer response.Body.Close()
	noneMatch := azcore.ETagAny
	contentType := "application/octet-stream"
	if response.ContentType != nil {
		contentType = *response.ContentType
	}
	_, err = s.client.UploadStream(ctx, s.container, finalObjectKey, response.Body, &azblob.UploadStreamOptions{
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &contentType},
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &noneMatch},
		},
	})
	if err != nil {
		return assets.BlobProperties{}, mapError(err)
	}
	if err := s.Delete(ctx, stagingObjectKey); err != nil {
		return assets.BlobProperties{}, err
	}
	return s.Inspect(ctx, finalObjectKey, "", 0)
}

func (s *Store) Open(ctx context.Context, objectKey string, requested assets.ByteRange, expectedETag string) (assets.BlobDownload, error) {
	options := &azblob.DownloadStreamOptions{}
	if requested.Offset > 0 || requested.Count > 0 {
		options.Range = blob.HTTPRange{Offset: requested.Offset, Count: requested.Count}
	}
	if expectedETag != "" {
		match := azcore.ETag(expectedETag)
		options.AccessConditions = &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &match}}
	}
	response, err := s.client.DownloadStream(ctx, s.container, objectKey, options)
	if err != nil {
		return assets.BlobDownload{}, mapError(err)
	}
	contentType := "application/octet-stream"
	if response.ContentType != nil {
		contentType = *response.ContentType
	}
	size := int64(0)
	if response.ContentLength != nil {
		size = *response.ContentLength
	}
	etag := ""
	if response.ETag != nil {
		etag = string(*response.ETag)
	}
	modified := time.Time{}
	if response.LastModified != nil {
		modified = *response.LastModified
	}
	totalSize := size
	if response.ContentRange != nil {
		totalSize = contentRangeTotal(*response.ContentRange, size)
	}
	return assets.BlobDownload{Body: response.Body, Size: size, TotalSize: totalSize, ContentType: contentType, ETag: etag, LastModified: modified}, nil
}

func (s *Store) Delete(ctx context.Context, objectKey string) error {
	_, err := s.client.DeleteBlob(ctx, s.container, objectKey, nil)
	err = mapError(err)
	if errors.Is(err, assets.ErrNotFound) {
		return nil
	}
	return err
}

func (s *Store) Put(ctx context.Context, objectKey string, reader io.Reader, _ int64, mimeType string) (assets.BlobProperties, error) {
	_, err := s.client.UploadStream(ctx, s.container, objectKey, reader, &azblob.UploadStreamOptions{HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &mimeType}})
	if err != nil {
		return assets.BlobProperties{}, mapError(err)
	}
	return s.Inspect(ctx, objectKey, "", 0)
}

func (s *Store) userDelegationCredential(ctx context.Context) (*service.UserDelegationCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delegation != nil && time.Until(s.delegationExpires) > 15*time.Minute {
		return s.delegation, nil
	}
	start := time.Now().UTC().Add(-5 * time.Minute)
	expiry := start.Add(6 * time.Hour)
	startText := start.Format(time.RFC3339)
	expiryText := expiry.Format(time.RFC3339)
	credential, err := s.client.ServiceClient().GetUserDelegationCredential(ctx, service.KeyInfo{Start: &startText, Expiry: &expiryText}, nil)
	if err != nil {
		return nil, err
	}
	s.delegation = credential
	s.delegationExpires = expiry
	return credential, nil
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	var responseError *azcore.ResponseError
	if errors.As(err, &responseError) {
		switch responseError.StatusCode {
		case http.StatusNotFound:
			return assets.ErrNotFound
		case http.StatusConflict:
			return assets.ErrConflict
		case http.StatusPreconditionFailed:
			return assets.ErrInvalidUpload
		case http.StatusRequestedRangeNotSatisfiable:
			return assets.ErrInvalidInput
		}
	}
	if strings.Contains(strings.ToLower(err.Error()), "blobnotfound") || strings.Contains(strings.ToLower(err.Error()), "statuscode=404") {
		return assets.ErrNotFound
	}
	return err
}

func contentRangeTotal(value string, fallback int64) int64 {
	index := strings.LastIndexByte(value, '/')
	if index < 0 {
		return fallback
	}
	total, err := strconv.ParseInt(value[index+1:], 10, 64)
	if err != nil || total < fallback {
		return fallback
	}
	return total
}
