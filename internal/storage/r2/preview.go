package r2

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

var packagePreviewKey = regexp.MustCompile(`^recordings/packages/[a-zA-Z0-9-]{1,80}/final/[a-zA-Z0-9-]{1,80}/previews/(current\.json|[a-zA-Z0-9-]{1,80}/(index\.vtt|seg-[0-9]{6}\.jpg))$`)
var previewPointer = regexp.MustCompile(`^\{"attempt":"[a-zA-Z0-9-]{1,80}"\}$`)

// Derived keys never enter the upload inventory or client signing paths.
// All writes are immutable, including the first-completed-attempt pointer.
func (s *Store) PutPackagePreview(ctx context.Context, key string, data []byte) error {
	if !packagePreviewKey.MatchString(key) || len(data) == 0 || len(data) > 1<<20 {
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
	return err
}
