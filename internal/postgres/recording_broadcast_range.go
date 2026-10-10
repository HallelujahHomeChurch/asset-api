package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"hhc/asset-api/internal/assets"
	"reflect"
)

func (s *RecordingCaptureStore) WithBroadcastObjects(objects assets.RecordingBroadcastObjects) *RecordingCaptureStore {
	s.broadcastObjects = objects
	return s
}

func scanBroadcastRange(row coverScanner) (*assets.RecordingBroadcastRange, error) {
	var p assets.RecordingBroadcastRange
	err := row.Scan(&p.RecordingID, &p.Epoch, &p.RangeRevision, &p.StartSequence, &p.EndSequenceExclusive, &p.Revoked, &p.MemberState)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

const broadcastRangeColumns = `recording_id,epoch,range_revision,start_sequence,end_sequence_exclusive,revoked,member_state`

func (s *RecordingCaptureStore) GetBroadcastRange(ctx context.Context, id string) (*assets.RecordingBroadcastRange, error) {
	return scanBroadcastRange(s.db.QueryRowContext(ctx, `SELECT `+broadcastRangeColumns+` FROM recording_broadcast_ranges WHERE capture_id=$1`, id))
}

func (s *RecordingCaptureStore) SetBroadcastRange(ctx context.Context, id string, next assets.RecordingBroadcastRange) (assets.RecordingBroadcastRange, error) {
	if err := assets.ValidateBroadcastRangeChange(nil, next); err != nil {
		return next, err
	}
	if s.broadcastObjects == nil {
		return next, errors.New("broadcast policy storage unavailable")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return next, err
	}
	defer tx.Rollback()
	if err = checkRecordingNotDeleted(ctx, tx, next.RecordingID); err != nil {
		return next, err
	}
	c, err := scanCapture(tx.QueryRowContext(ctx, `SELECT `+captureColumns+` FROM recording_captures WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return next, err
	}
	if c.RecordingID != next.RecordingID || c.BroadcastEpoch != next.Epoch || c.BroadcastEpoch == 0 {
		return next, assets.ErrConflict
	}
	var latestEpoch int64
	if err = tx.QueryRowContext(ctx, `SELECT max(epoch) FROM recording_broadcast_ranges WHERE recording_id=$1`, next.RecordingID).Scan(&latestEpoch); err != nil {
		return next, err
	}
	if latestEpoch != next.Epoch || c.TerminalAt != nil && !next.Revoked {
		return next, assets.ErrConflict
	}
	current, err := scanBroadcastRange(tx.QueryRowContext(ctx, `SELECT `+broadcastRangeColumns+` FROM recording_broadcast_ranges WHERE capture_id=$1 FOR UPDATE`, id))
	if err != nil {
		return next, err
	}
	if current == nil {
		return next, assets.ErrConflict
	}
	if err = assets.ValidateBroadcastRangeChange(current, next); err != nil {
		return next, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_broadcast_ranges SET range_revision=$2,start_sequence=$3,end_sequence_exclusive=$4,revoked=$5,member_state=$6 WHERE capture_id=$1`, id, next.RangeRevision, next.StartSequence, next.EndSequenceExclusive, next.Revoked, next.MemberState); err != nil {
		return next, err
	}
	// Hold the capture/range lock until the conditional authority write completes.
	// A failed DB commit can only invalidate signed grants, never expose more bytes.
	if err = s.broadcastObjects.PutBroadcastRange(ctx, id, broadcastRangeJSON(next)); err != nil {
		return next, err
	}
	return next, tx.Commit()
}

func (s *RecordingCaptureStore) GetBroadcastProjection(ctx context.Context, id string) (assets.RecordingBroadcastProjection, error) {
	var raw []byte
	var revision int64
	var last int
	var ended, terminal bool
	var p assets.RecordingBroadcastRange
	err := s.db.QueryRowContext(ctx, `SELECT b.recording_id,b.epoch,b.range_revision,b.start_sequence,b.end_sequence_exclusive,b.revoked,b.member_state,l.segments,l.published_revision,l.published_sequence,l.ended AND l.published_revision=l.revision,c.terminal_at IS NOT NULL OR c.state IN ('failed','expired','aborted') FROM recording_broadcast_ranges b JOIN recording_live l ON l.capture_id=b.capture_id JOIN recording_captures c ON c.id=b.capture_id WHERE b.capture_id=$1`, id).Scan(&p.RecordingID, &p.Epoch, &p.RangeRevision, &p.StartSequence, &p.EndSequenceExclusive, &p.Revoked, &p.MemberState, &raw, &revision, &last, &ended, &terminal)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.RecordingBroadcastProjection{}, assets.ErrNotFound
	}
	if err != nil {
		return assets.RecordingBroadcastProjection{}, err
	}
	var history []assets.RecordingLiveSegment
	if err = json.Unmarshal(raw, &history); err != nil {
		return assets.RecordingBroadcastProjection{}, err
	}
	// The newest DB batch may precede its R2 publication; only published media counts.
	n := min(len(history), last+1)
	if s.broadcastObjects == nil {
		return assets.RecordingBroadcastProjection{}, errors.New("broadcast policy storage unavailable")
	}
	remoteJSON, err := s.broadcastObjects.ReadBroadcastRange(ctx, id)
	if err != nil {
		return assets.RecordingBroadcastProjection{}, err
	}
	var remote assets.RecordingBroadcastRange
	if err = json.Unmarshal(remoteJSON, &remote); err != nil {
		return assets.RecordingBroadcastProjection{}, err
	}
	if !reflect.DeepEqual(p, remote) {
		return assets.RecordingBroadcastProjection{}, assets.ErrConflict
	}
	projection := assets.BroadcastProjection(p, history[:n], revision, ended)
	if terminal {
		projection.State = "failed"
		projection.DurationSeconds = nil
		projection.MediaStartSeconds = nil
	}
	return projection, nil
}

func broadcastRangeJSON(p assets.RecordingBroadcastRange) []byte {
	data, _ := json.Marshal(p)
	return data
}
