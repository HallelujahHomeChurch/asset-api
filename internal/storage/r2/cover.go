package r2

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"io"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

var recordingCoverKey = regexp.MustCompile(`^recordings/covers/[a-zA-Z0-9-]{1,80}/[a-zA-Z0-9-]{1,80}/(auto-[123]|custom)\.jpg$`)
var recordingCoverInputKey = regexp.MustCompile(`^recordings/covers/[a-zA-Z0-9-]{1,80}/[a-zA-Z0-9-]{1,80}/input$`)

func (s *Store) PutCoverInput(ctx context.Context, key string, data []byte, mime string) error {
	if !recordingCoverInputKey.MatchString(key) || len(data) == 0 || len(data) > 5<<20 || (mime != "image/jpeg" && mime != "image/png") {
		return errors.New("invalid cover input")
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String(mime), CacheControl: aws.String("private, no-store"), IfNoneMatch: aws.String("*")})
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() == "PreconditionFailed" {
		body, openErr := s.Open(ctx, key)
		if openErr != nil {
			return openErr
		}
		defer body.Close()
		current, readErr := io.ReadAll(io.LimitReader(body, (5<<20)+1))
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(current, data) {
			return errors.New("cover input conflict")
		}
		return nil
	}
	return err
}

func (s *Store) DeleteCoverObjects(ctx context.Context, keys []string) error {
	if len(keys) == 0 || len(keys) > 1000 {
		return errors.New("invalid cover delete")
	}
	var recording string
	objects := make([]types.ObjectIdentifier, 0, len(keys))
	for _, key := range keys {
		if !recordingCoverKey.MatchString(key) && !recordingCoverInputKey.MatchString(key) {
			return errors.New("invalid cover delete key")
		}
		id := strings.Split(key, "/")[2]
		if recording != "" && recording != id {
			return errors.New("mixed cover delete scope")
		}
		recording = id
		objects = append(objects, types.ObjectIdentifier{Key: aws.String(key)})
	}
	result, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(s.bucket), Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)}})
	if err != nil {
		return err
	}
	if len(result.Errors) > 0 {
		return errors.New("cover delete incomplete")
	}
	return nil
}

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
