package assets

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
)

const BroadcastMaxRevision int64 = 9007199254740991

type RecordingBroadcastRange struct {
	RecordingID          string `json:"recordingId"`
	Epoch                int64  `json:"epoch"`
	RangeRevision        int64  `json:"rangeRevision"`
	StartSequence        *int   `json:"startSequence"`
	EndSequenceExclusive *int   `json:"endSequenceExclusive"`
	Revoked              bool   `json:"revoked"`
	MemberState          string `json:"memberState"`
}

func (p *RecordingBroadcastRange) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || (len(fields) != 6 && len(fields) != 7) || bytes.Equal(fields["revoked"], []byte("null")) || bytes.Equal(fields["memberState"], []byte("null")) {
		return ErrInvalidInput
	}
	for _, name := range []string{"recordingId", "epoch", "rangeRevision", "startSequence", "endSequenceExclusive", "revoked"} {
		if _, ok := fields[name]; !ok {
			return ErrInvalidInput
		}
	}
	type wire RecordingBroadcastRange
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	p.MemberState = "blocked"
	return decoder.Decode((*wire)(p))
}

type RecordingBroadcastProjection struct {
	Epoch                int64    `json:"epoch"`
	RangeRevision        int64    `json:"rangeRevision"`
	State                string   `json:"state"`
	StartSequence        *int     `json:"startSequence"`
	EndSequenceExclusive *int     `json:"endSequenceExclusive"`
	MediaStartSeconds    *float64 `json:"mediaStartSeconds"`
	DurationSeconds      *float64 `json:"durationSeconds"`
	Revision             int64    `json:"revision"`
}
type RecordingBroadcastObjects interface {
	PutBroadcastRange(context.Context, string, []byte) error
	ReadBroadcastRange(context.Context, string) ([]byte, error)
}
type RecordingBroadcastRepository interface {
	SetBroadcastRange(context.Context, string, RecordingBroadcastRange) (RecordingBroadcastRange, error)
	GetBroadcastRange(context.Context, string) (*RecordingBroadcastRange, error)
	GetBroadcastProjection(context.Context, string) (RecordingBroadcastProjection, error)
}

func ValidateBroadcastRangeChange(current *RecordingBroadcastRange, next RecordingBroadcastRange) error {
	if next.MemberState != "blocked" && next.MemberState != "live" && next.MemberState != "vod" {
		return ErrInvalidInput
	}
	if !captureUUID.MatchString(next.RecordingID) || next.Epoch < 1 || next.Epoch > BroadcastMaxRevision || next.RangeRevision < 1 || next.RangeRevision > BroadcastMaxRevision {
		return ErrInvalidInput
	}
	for _, v := range []*int{next.StartSequence, next.EndSequenceExclusive} {
		if v != nil && (*v < 0 || int64(*v) > 2147483647) {
			return ErrInvalidInput
		}
	}
	if next.EndSequenceExclusive != nil && (next.StartSequence == nil || *next.EndSequenceExclusive <= *next.StartSequence) {
		return ErrInvalidInput
	}
	if current == nil {
		return nil
	}
	if next.Epoch != current.Epoch || next.RecordingID != current.RecordingID || next.RangeRevision < current.RangeRevision {
		return ErrConflict
	}
	if next.RangeRevision == current.RangeRevision {
		if !reflect.DeepEqual(current, &next) {
			return ErrConflict
		}
		return nil
	}
	if current.StartSequence != nil && !reflect.DeepEqual(current.StartSequence, next.StartSequence) || current.EndSequenceExclusive != nil && !reflect.DeepEqual(current.EndSequenceExclusive, next.EndSequenceExclusive) || current.Revoked && !next.Revoked {
		return ErrConflict
	}
	return nil
}

func BroadcastProjection(policy RecordingBroadcastRange, history []RecordingLiveSegment, revision int64, ended bool) RecordingBroadcastProjection {
	p := RecordingBroadcastProjection{Epoch: policy.Epoch, RangeRevision: policy.RangeRevision, State: "pending", StartSequence: policy.StartSequence, EndSequenceExclusive: policy.EndSequenceExclusive, Revision: max(1, revision)}
	if policy.Revoked {
		p.State = "failed"
		return p
	}
	if policy.StartSequence == nil {
		return p
	}
	start := *policy.StartSequence
	end := len(history)
	if policy.EndSequenceExclusive != nil {
		end = *policy.EndSequenceExclusive
	}
	if start >= len(history) || end > len(history) {
		if ended {
			p.State = "failed"
		}
		return p
	}
	if end <= start {
		p.State = "failed"
		return p
	}
	origin := history[start].Renditions["1080p"].Start
	duration := history[end-1].Renditions["1080p"].End - origin
	p.MediaStartSeconds = &origin
	p.DurationSeconds = &duration
	p.State = "ready"
	return p
}
