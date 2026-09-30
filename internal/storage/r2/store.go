package r2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const maxParts = 597

var ErrNotFound = errors.New("R2 object not found")

var namePattern = regexp.MustCompile(`^[a-z0-9-]+$`)
var packageStagingKey = regexp.MustCompile(`^recordings/packages/[a-zA-Z0-9-]{1,80}/staging/(master\.m3u8|(720p|1080p)/(index\.m3u8|init\.mp4|seg-[0-9]{6}\.m4s))$`)

type Store struct {
	bucket  string
	client  *s3.Client
	presign *s3.PresignClient
}

type PresignedPart struct {
	URL     string              `json:"url"`
	Method  string              `json:"method"`
	Headers map[string][]string `json:"headers"`
}

type Part struct {
	Number int
	ETag   string
	Size   int64
}

func New(accountID, bucket, accessKey, secretKey string) (*Store, error) {
	if !namePattern.MatchString(accountID) || !namePattern.MatchString(bucket) || accessKey == "" || secretKey == "" {
		return nil, errors.New("invalid R2 configuration")
	}
	client := s3.NewFromConfig(aws.Config{
		Region:                     "auto",
		Credentials:                credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String("https://" + accountID + ".r2.cloudflarestorage.com")
		options.UsePathStyle = true
	})
	return &Store{bucket: bucket, client: client, presign: s3.NewPresignClient(client)}, nil
}

func validKey(key string) bool {
	return strings.HasPrefix(key, "recordings/") && !strings.Contains(key, "..") && !strings.ContainsAny(key, "?#\\")
}

// No final key can receive a client PUT capability. Hash validation is done
// against the immutable copy by the worker, not trusted from upload metadata.
func (s *Store) PresignPackageObject(ctx context.Context, key string, size int64, contentType string, ttl time.Duration) (PresignedPart, error) {
	if !packageStagingKey.MatchString(key) || size <= 0 || size > 128<<20 || ttl <= 0 || ttl > 15*time.Minute || (contentType != "video/mp4" && contentType != "application/vnd.apple.mpegurl") {
		return PresignedPart{}, errors.New("invalid recording package object")
	}
	result, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), ContentLength: aws.Int64(size), ContentType: aws.String(contentType),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return PresignedPart{}, fmt.Errorf("presign R2 package object: %w", err)
	}
	return PresignedPart{URL: result.URL, Method: result.Method, Headers: result.SignedHeader}, nil
}

func (s *Store) Create(ctx context.Context, key string) (string, error) {
	if !validKey(key) {
		return "", errors.New("invalid recording key")
	}
	result, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), ContentType: aws.String("video/mp4"),
	})
	if err != nil {
		return "", fmt.Errorf("start R2 multipart: %w", err)
	}
	return aws.ToString(result.UploadId), nil
}

func (s *Store) PresignPart(ctx context.Context, key, uploadID string, partNumber int, ttl time.Duration) (PresignedPart, error) {
	if !validKey(key) || uploadID == "" || partNumber < 1 || partNumber > maxParts || ttl <= 0 || ttl > 15*time.Minute {
		return PresignedPart{}, errors.New("invalid recording part request")
	}
	result, err := s.presign.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(uploadID), PartNumber: aws.Int32(int32(partNumber)),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return PresignedPart{}, fmt.Errorf("presign R2 part: %w", err)
	}
	return PresignedPart{URL: result.URL, Method: result.Method, Headers: result.SignedHeader}, nil
}

func (s *Store) PresignProbeRead(ctx context.Context, key string, ttl time.Duration) (PresignedPart, error) {
	if !validKey(key) || ttl <= 0 || ttl > 15*time.Minute {
		return PresignedPart{}, errors.New("invalid recording probe request")
	}
	result, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return PresignedPart{}, fmt.Errorf("presign R2 probe: %w", err)
	}
	return PresignedPart{URL: result.URL, Method: result.Method, Headers: result.SignedHeader}, nil
}

func (s *Store) ListParts(ctx context.Context, key, uploadID string) ([]Part, error) {
	if !validKey(key) || uploadID == "" {
		return nil, errors.New("invalid recording upload")
	}
	result, err := s.client.ListParts(ctx, &s3.ListPartsInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(uploadID), MaxParts: aws.Int32(maxParts),
	})
	if err != nil {
		return nil, fmt.Errorf("list R2 parts: %w", err)
	}
	if aws.ToBool(result.IsTruncated) {
		return nil, errors.New("recording part limit exceeded")
	}
	parts := make([]Part, 0, len(result.Parts))
	for _, part := range result.Parts {
		parts = append(parts, Part{Number: int(aws.ToInt32(part.PartNumber)), ETag: aws.ToString(part.ETag), Size: aws.ToInt64(part.Size)})
	}
	return parts, nil
}

func (s *Store) Complete(ctx context.Context, key, uploadID string, parts []Part) error {
	if !validKey(key) || uploadID == "" || len(parts) == 0 || len(parts) > maxParts {
		return errors.New("invalid recording completion")
	}
	completed := make([]types.CompletedPart, 0, len(parts))
	for index, part := range parts {
		if part.Number != index+1 || part.ETag == "" {
			return errors.New("invalid recording part order")
		}
		completed = append(completed, types.CompletedPart{PartNumber: aws.Int32(int32(part.Number)), ETag: aws.String(part.ETag)})
	}
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		return fmt.Errorf("complete R2 multipart: %w", err)
	}
	return nil
}

func (s *Store) Abort(ctx context.Context, key, uploadID string) error {
	if !validKey(key) || uploadID == "" {
		return errors.New("invalid recording upload")
	}
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	var missing *types.NoSuchUpload
	if errors.As(err, &missing) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("abort R2 multipart: %w", err)
	}
	return nil
}

func (s *Store) Head(ctx context.Context, key string) (int64, string, error) {
	if !validKey(key) {
		return 0, "", errors.New("invalid recording key")
	}
	result, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		var missing *types.NotFound
		if errors.As(err, &missing) {
			return 0, "", ErrNotFound
		}
		return 0, "", fmt.Errorf("head R2 object: %w", err)
	}
	return aws.ToInt64(result.ContentLength), aws.ToString(result.ETag), nil
}

func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if !validKey(key) {
		return nil, errors.New("invalid recording key")
	}
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("open R2 object: %w", err)
	}
	return result.Body, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	if !validKey(key) {
		return errors.New("invalid recording key")
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("delete R2 object: %w", err)
	}
	return nil
}
