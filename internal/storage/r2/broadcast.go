package r2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func broadcastPolicyKey(id string) string {
	return "recordings/captures/" + id + "/final/broadcast.json"
}
func parseBroadcastRange(raw []byte) (broadcastRange, error) {
	var p = broadcastRange{MemberState: "blocked"}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || (len(fields) != 6 && len(fields) != 7) || bytes.Equal(fields["revoked"], []byte("null")) || bytes.Equal(fields["memberState"], []byte("null")) {
		return p, ErrLiveConflict
	}
	for _, name := range []string{"recordingId", "epoch", "rangeRevision", "startSequence", "endSequenceExclusive", "revoked"} {
		if _, ok := fields[name]; !ok {
			return p, ErrLiveConflict
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) > 1024 || d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF || validateBroadcastRangeChange(nil, p) != nil {
		return p, errors.New("invalid broadcast authority")
	}
	return p, nil
}
func (s *Store) ReadBroadcastRange(ctx context.Context, id string) ([]byte, error) {
	if !sourceAttemptID.MatchString(id) {
		return nil, ErrLiveConflict
	}
	object, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(broadcastPolicyKey(id))})
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(object.Body, 1025))
	err = errors.Join(err, object.Body.Close())
	if err != nil {
		return nil, err
	}
	if _, err = parseBroadcastRange(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// Conditional replacement prevents delayed retries from restoring an older scope.
func (s *Store) PutBroadcastRange(ctx context.Context, id string, data []byte) error {
	next, err := parseBroadcastRange(data)
	if err != nil {
		return err
	}
	if !sourceAttemptID.MatchString(id) || validateBroadcastRangeChange(nil, next) != nil {
		return ErrLiveConflict
	}

	for attempt := 0; attempt < 3; attempt++ {
		object, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(broadcastPolicyKey(id))})
		input := &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(broadcastPolicyKey(id)), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String("application/json"), CacheControl: aws.String("private, no-store")}
		if err == nil {
			raw, readErr := io.ReadAll(io.LimitReader(object.Body, 1025))
			if err = errors.Join(readErr, object.Body.Close()); err != nil {
				return err
			}
			previous, err := parseBroadcastRange(raw)
			if err != nil {
				return err
			}
			if err = validateBroadcastRangeChange(&previous, next); err != nil {
				return err
			}
			if previous.RangeRevision == next.RangeRevision {
				return nil
			}
			if aws.ToString(object.ETag) == "" {
				return errors.New("missing authority etag")
			}
			input.IfMatch = object.ETag
		} else {
			var api smithy.APIError
			if !errors.As(err, &api) || api.ErrorCode() != "NoSuchKey" && api.ErrorCode() != "NotFound" {
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

type broadcastRange struct {
	RecordingID          string `json:"recordingId"`
	Epoch                int64  `json:"epoch"`
	RangeRevision        int64  `json:"rangeRevision"`
	StartSequence        *int   `json:"startSequence"`
	EndSequenceExclusive *int   `json:"endSequenceExclusive"`
	Revoked              bool   `json:"revoked"`
	MemberState          string `json:"memberState"`
}

func validateBroadcastRangeChange(current *broadcastRange, next broadcastRange) error {
	if next.MemberState != "blocked" && next.MemberState != "live" && next.MemberState != "vod" {
		return ErrLiveConflict
	}
	if !regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`).MatchString(next.RecordingID) || next.Epoch < 1 || next.Epoch > 9007199254740991 || next.RangeRevision < 1 || next.RangeRevision > 9007199254740991 {
		return ErrLiveConflict
	}
	for _, n := range []*int{next.StartSequence, next.EndSequenceExclusive} {
		if n != nil && (*n < 0 || int64(*n) > 2147483647) {
			return ErrLiveConflict
		}
	}
	if next.EndSequenceExclusive != nil && (next.StartSequence == nil || *next.EndSequenceExclusive <= *next.StartSequence) {
		return ErrLiveConflict
	}
	if current != nil && (current.RecordingID != next.RecordingID || current.Epoch != next.Epoch || next.RangeRevision < current.RangeRevision || next.RangeRevision == current.RangeRevision && !reflect.DeepEqual(*current, next) || current.StartSequence != nil && !reflect.DeepEqual(current.StartSequence, next.StartSequence) || current.EndSequenceExclusive != nil && !reflect.DeepEqual(current.EndSequenceExclusive, next.EndSequenceExclusive) || current.Revoked && !next.Revoked) {
		return ErrLiveConflict
	}
	return nil
}
