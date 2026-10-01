package r2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

var ErrNotFound = errors.New("R2 object not found")

var namePattern = regexp.MustCompile(`^[a-z0-9-]+$`)
var packageStagingKey = regexp.MustCompile(`^recordings/packages/[a-zA-Z0-9-]{1,80}/staging/(master\.m3u8|(720p|1080p)/(index\.m3u8|init\.mp4|seg-[0-9]{6}\.m4s))$`)
var packageFinalKey = regexp.MustCompile(`^recordings/packages/[a-zA-Z0-9-]{1,80}/final/[a-zA-Z0-9-]{1,80}/(master\.m3u8|(720p|1080p)/(index\.m3u8|init\.mp4|seg-[0-9]{6}\.m4s))$`)
var packageControlKey = regexp.MustCompile(`^recordings/packages/[a-zA-Z0-9-]{1,80}/final/[a-zA-Z0-9-]{1,80}/package\.json$`)
var sourceAttemptID = regexp.MustCompile(`^[a-f0-9]{32}$`)

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

func (s *Store) CopyPackageObject(ctx context.Context, source, destination, sourceETag string) error {
	if !packageStagingKey.MatchString(source) || !packageFinalKey.MatchString(destination) || sourceETag == "" || strings.ContainsAny(sourceETag, "\r\n") {
		return errors.New("invalid package copy")
	}
	from, to := strings.SplitN(source, "/", 5), strings.SplitN(destination, "/", 6)
	if from[2] != to[2] || from[4] != to[5] {
		return errors.New("invalid package copy scope")
	}
	contentType := "video/mp4"
	if strings.HasSuffix(destination, ".m3u8") {
		contentType = "application/vnd.apple.mpegurl"
	}
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(destination), CopySource: aws.String(url.PathEscape(s.bucket + "/" + source)), CopySourceIfMatch: aws.String(sourceETag),
		MetadataDirective: types.MetadataDirectiveReplace, ContentType: aws.String(contentType), CacheControl: aws.String("private, max-age=0"),
	})
	if err != nil {
		return fmt.Errorf("copy R2 package object: %w", err)
	}
	return nil
}

func (s *Store) PutPackageInventory(ctx context.Context, key string, data []byte) error {
	if !packageControlKey.MatchString(key) || len(data) == 0 || len(data) > 8<<20 || !json.Valid(data) {
		return errors.New("invalid package inventory write")
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String("application/json"), CacheControl: aws.String("private, no-store")})
	if err != nil {
		return fmt.Errorf("write R2 package inventory: %w", err)
	}
	return nil
}

// PutRecordingObject is only for a source Job's bounded, seekable spool. It
// cannot overwrite an object or write directly into a playable final prefix.
func (s *Store) PutRecordingObject(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType string) error {
	if !packageStagingKey.MatchString(key) || body == nil || size <= 0 || size > 128<<20 {
		return errors.New("invalid recording output")
	}
	wantType := "video/mp4"
	if strings.HasSuffix(key, ".m3u8") {
		wantType = "application/vnd.apple.mpegurl"
		if size > 1<<20 {
			return errors.New("invalid recording playlist output")
		}
	}
	if contentType != wantType {
		return errors.New("invalid recording output content type")
	}
	position, err := body.Seek(0, io.SeekCurrent)
	if err != nil || position != 0 {
		return errors.New("invalid recording output position")
	}
	length, err := body.Seek(0, io.SeekEnd)
	if err != nil || length != size {
		return errors.New("invalid recording output length")
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return errors.New("invalid recording output seek")
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: body, ContentLength: aws.Int64(size), ContentType: aws.String(contentType), IfNoneMatch: aws.String("*"), CacheControl: aws.String("private, no-store")})
	if err != nil {
		return errors.New("recording output upload failed")
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

// DeletePackageObjects is bounded to one server-owned staging/attempt scope.
// A successful HTTP response may still contain per-object deletion failures.
func (s *Store) DeletePackageObjects(ctx context.Context, keys []string) error {
	if len(keys) == 0 || len(keys) > 1000 {
		return errors.New("invalid package delete batch")
	}
	var scope string
	seen := map[string]bool{}
	objects := make([]types.ObjectIdentifier, 0, len(keys))
	for _, key := range keys {
		parts := strings.SplitN(key, "/", 6)
		current := ""
		if packageStagingKey.MatchString(key) {
			current = strings.Join(parts[:4], "/") + "/"
		} else if packageFinalKey.MatchString(key) || packageControlKey.MatchString(key) {
			current = strings.Join(parts[:5], "/") + "/"
		} else {
			return errors.New("invalid package delete key")
		}
		if seen[key] || scope != "" && current != scope {
			return errors.New("mixed package delete scope")
		}
		seen[key] = true
		scope = current
		objects = append(objects, types.ObjectIdentifier{Key: aws.String(key)})
	}
	result, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(s.bucket), Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)}})
	if err != nil {
		return fmt.Errorf("delete R2 package objects: %w", err)
	}
	if len(result.Errors) > 0 {
		return errors.New("R2 package delete incomplete")
	}
	return nil
}

// Failed source encoding has no complete inventory. List only that durable
// attempt's two private prefixes and reuse the guarded batched delete path.
func (s *Store) DeleteSourceAttempt(ctx context.Context, id string) error {
	if !sourceAttemptID.MatchString(id) {
		return errors.New("invalid source attempt")
	}
	for _, prefix := range []string{"recordings/packages/" + id + "/staging/", "recordings/packages/" + id + "/final/" + id + "/"} {
		pager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(1000)})
		count, pages := 0, 0
		for pager.HasMorePages() {
			pages++
			if pages > 11 {
				return errors.New("source attempt listing exceeds page limit")
			}
			page, err := pager.NextPage(ctx)
			if err != nil {
				return errors.New("source attempt listing failed")
			}
			count += len(page.Contents)
			if count > 10001 || len(page.Contents) > 1000 {
				return errors.New("source attempt listing exceeds limit")
			}
			keys := make([]string, 0, len(page.Contents))
			for _, object := range page.Contents {
				key := aws.ToString(object.Key)
				if !strings.HasPrefix(key, prefix) {
					return errors.New("source attempt listing escaped scope")
				}
				keys = append(keys, key)
			}
			if len(keys) > 0 {
				if err := s.DeletePackageObjects(ctx, keys); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
