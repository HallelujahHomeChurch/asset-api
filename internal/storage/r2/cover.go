package r2

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var recordingCoverKey = regexp.MustCompile(`^recordings/covers/[a-zA-Z0-9-]{1,80}/[a-zA-Z0-9-]{1,80}/(auto-[123]|custom)\.jpg$`)

// PutRecordingCover only accepts worker-produced immutable JPEGs. It cannot
// write package objects or mint public/client upload capabilities.
func (s *Store) PutRecordingCover(ctx context.Context, key string, data []byte) error {
	if !recordingCoverKey.MatchString(key) || len(data) == 0 || len(data) > 1<<20 {
		return errors.New("invalid cover write")
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != 1280 || cfg.Height != 720 {
		return errors.New("invalid cover JPEG")
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String("image/jpeg"), CacheControl: aws.String("private, no-store"), IfNoneMatch: aws.String("*")})
	return err
}
