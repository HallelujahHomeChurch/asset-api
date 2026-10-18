package r2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

var packagePreviewKey = regexp.MustCompile(`^recordings/packages/[a-zA-Z0-9-]{1,80}/final/[a-zA-Z0-9-]{1,80}/previews/(current\.json|[a-zA-Z0-9-]{1,80}/(index\.vtt|seg-[0-9]{6}\.jpg))$`)
var livePreviewKey = regexp.MustCompile(`^recordings/captures/[a-f0-9]{32}/final/previews/(current\.json|shared/seg-[0-9]{6}\.(jpg|vtt)|[1-9][0-9]{0,3}/index\.vtt)$`)

var previewPointer = regexp.MustCompile(`^\{"attempt":"[a-zA-Z0-9-]{1,80}"\}$`)

// Derived keys never enter the upload inventory or client signing paths.
// All writes are immutable, including the first-completed-attempt pointer.
func (s *Store) PutPackagePreview(ctx context.Context, key string, data []byte) error {
	limit := 1 << 20
	if strings.HasSuffix(key, ".vtt") {
		limit = 2 << 20
	}
	if (!packagePreviewKey.MatchString(key) && !livePreviewKey.MatchString(key)) || len(data) == 0 || len(data) > limit {
		return errors.New("invalid preview write")
	}
	mime := "image/jpeg"
	if strings.HasSuffix(key, ".vtt") {
		mime = "text/vtt"
		if !bytes.HasPrefix(data, []byte("WEBVTT\n")) {
			return errors.New("invalid preview index")
		}
	}
	pointer := strings.HasSuffix(key, "/current.json")
	if pointer {
		mime = "application/json"
		if !previewPointer.Match(data) {
			return errors.New("invalid preview pointer")
		}
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String(mime), CacheControl: aws.String("private, no-store"), IfNoneMatch: aws.String("*")})
	var api smithy.APIError
	// A previous complete attempt may have published before losing its DB
	// response. Its immutable pointer is already a valid completion receipt.
	if pointer && errors.As(err, &api) && api.ErrorCode() == "PreconditionFailed" {
		return nil
	}
	if !pointer && livePreviewKey.MatchString(key) && preconditionFailed(err) {
		body, e := s.Open(ctx, key)
		if e != nil {
			return e
		}
		existing, e := io.ReadAll(io.LimitReader(body, int64(len(data))+1))
		e = errors.Join(e, body.Close())
		if e != nil {
			return e
		}
		if !bytes.Equal(existing, data) {
			return ErrLiveConflict
		}
		return nil
	}
	return err
}
