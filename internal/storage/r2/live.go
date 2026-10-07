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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

var liveObjectKey = regexp.MustCompile(`^recordings/captures/([a-f0-9]{32})/final/((480p|720p|1080p)/(init\.mp4|seg-[0-9]{6}\.m4s))$`)
var liveDeleteKey = regexp.MustCompile(`^recordings/captures/[a-f0-9]{32}/final/(current\.json|(480p|720p|1080p)/(init\.mp4|seg-[0-9]{6}\.m4s)|playlists/[1-9][0-9]{0,3}/(master\.m3u8|(480p|720p|1080p)/index\.m3u8))$`)
var livePlaylistName = regexp.MustCompile(`^(master\.m3u8|(480p|720p|1080p)/index\.m3u8)$`)
var ErrLiveConflict = errors.New("live pointer conflict")

// PublishLiveObject accepts only an already hashed/decoded server-owned attempt.
// Every retry of a declared object has identical bytes before it reaches here.
func (s *Store) PublishLiveObject(ctx context.Context, source, destination, etag string) error {
	match := liveObjectKey.FindStringSubmatch(destination)
	parts := strings.SplitN(source, "/", 6)
	if !packageFinalKey.MatchString(source) || len(match) == 0 || len(parts) != 6 || parts[2] != match[1] || parts[5] != match[2] || etag == "" || strings.ContainsAny(etag, "\r\n") {
		return errors.New("invalid live copy")
	}
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(destination), CopySource: aws.String(url.PathEscape(s.bucket + "/" + source)), CopySourceIfMatch: aws.String(etag), MetadataDirective: types.MetadataDirectiveReplace, ContentType: aws.String("video/mp4"), CacheControl: aws.String("private, no-store")})
	return err
}

func (s *Store) PutLivePlaylist(ctx context.Context, id string, revision int64, name string, data []byte) error {
	if !sourceAttemptID.MatchString(id) || revision < 1 || revision > 1441 || !livePlaylistName.MatchString(name) || len(data) < 8 || len(data) > 1<<20 || !bytes.HasPrefix(data, []byte("#EXTM3U\n")) {
		return errors.New("invalid live playlist")
	}
	key := fmt.Sprintf("recordings/captures/%s/final/playlists/%d/%s", id, revision, name)
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String("application/vnd.apple.mpegurl"), CacheControl: aws.String("private, no-store"), IfNoneMatch: aws.String("*")})
	if !preconditionFailed(err) {
		return err
	}
	// A lost success can replay, but an immutable revision cannot change content.
	body, err := s.Open(ctx, key)
	if err != nil {
		return err
	}
	existing, readErr := io.ReadAll(io.LimitReader(body, int64(len(data))+1))
	err = errors.Join(readErr, body.Close())
	if err != nil {
		return err
	}
	if !bytes.Equal(existing, data) {
		return ErrLiveConflict
	}
	return nil
}

type livePointer struct {
	Revision     int64 `json:"revision"`
	LastSequence int   `json:"lastSequence"`
}

// R2 conditional PUT fences delayed publishers. Replaying an older revision
// cannot overwrite a newer common waterline, including after lease loss.
func (s *Store) AdvanceLivePointer(ctx context.Context, id string, revision int64, lastSequence int) error {
	if !sourceAttemptID.MatchString(id) || revision < 1 || revision > 1441 || lastSequence < 0 || lastSequence > 1439 {
		return errors.New("invalid live pointer")
	}
	key := "recordings/captures/" + id + "/final/current.json"
	data, _ := json.Marshal(livePointer{revision, lastSequence})
	for attempt := 0; attempt < 3; attempt++ {
		current, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
		input := &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String("application/json"), CacheControl: aws.String("private, no-store")}
		if err == nil {
			raw, readErr := io.ReadAll(io.LimitReader(current.Body, 129))
			closeErr := current.Body.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				return err
			}
			var previous livePointer
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if len(raw) > 128 || decoder.Decode(&previous) != nil || decoder.Decode(new(any)) != io.EOF || previous.Revision < 1 || previous.Revision > 1441 || previous.LastSequence < 0 || previous.LastSequence > 1439 || aws.ToString(current.ETag) == "" {
				return errors.New("invalid stored live pointer")
			}
			if previous.Revision >= revision {
				if previous.LastSequence < lastSequence || (previous.Revision == revision && previous.LastSequence != lastSequence) {
					return ErrLiveConflict
				}
				return nil
			}
			if previous.LastSequence > lastSequence {
				return ErrLiveConflict
			}
			input.IfMatch = current.ETag
		} else {
			var api smithy.APIError
			if !errors.As(err, &api) || (api.ErrorCode() != "NoSuchKey" && api.ErrorCode() != "NotFound") {
				return err
			}
			input.IfNoneMatch = aws.String("*")
		}
		_, err = s.client.PutObject(ctx, input)
		if !preconditionFailed(err) {
			return err
		}
	}
	return ErrLiveConflict
}
func preconditionFailed(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && (api.ErrorCode() == "PreconditionFailed" || api.ErrorCode() == "ConditionalRequestConflict")
}
