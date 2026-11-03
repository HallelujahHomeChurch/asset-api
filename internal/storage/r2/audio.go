package r2

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var packageAudioKey = regexp.MustCompile(`^recordings/packages/[a-zA-Z0-9-]{1,80}/final/[a-zA-Z0-9-]{1,80}/audio/aac-hls-v1/(current\.json|[a-zA-Z0-9-]{1,80}/(index\.m3u8|seg-[0-9]{6}\.ts))$`)

func (s *Store) PutPackageAudio(ctx context.Context, key string, data []byte) error {
	limit := 8 << 20
	mime := "video/mp2t"
	if strings.HasSuffix(key, ".m3u8") {
		limit = 1 << 20
		mime = "application/vnd.apple.mpegurl"
	}
	pointer := strings.HasSuffix(key, "/current.json")
	if pointer {
		limit = 128
		mime = "application/json"
	}
	if !packageAudioKey.MatchString(key) || len(data) == 0 || len(data) > limit || pointer && !previewPointer.Match(data) || strings.HasSuffix(key, ".m3u8") && !bytes.HasPrefix(data, []byte("#EXTM3U\n")) {
		return errors.New("invalid audio write")
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String(mime), CacheControl: aws.String("private, no-store"), IfNoneMatch: aws.String("*")})
	// A completed immutable attempt may have won before its DB response was lost.
	if pointer && preconditionFailed(err) {
		return nil
	}
	return err
}
