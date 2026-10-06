package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"hhc/asset-api/internal/assets"
)

func initialRecordingProgress(attempt int, phase string, at time.Time) []byte {
	data, _ := json.Marshal(assets.RecordingProcessingProgress{Attempt: attempt, Phase: phase, AttemptStartedAt: at, PhaseStartedAt: at, LastProgressAt: at, HeartbeatAt: at})
	return data
}

func (s *RecordingPackageStore) UpdatePackageProcessingProgress(ctx context.Context, id, claimID string, value assets.RecordingProcessingProgress) error {
	if err := assets.ValidateRecordingProcessingProgress(value); err != nil {
		return err
	}
	return s.packageClaimTransaction(ctx, id, claimID, func(tx *sql.Tx) error {
		return updateRecordingProgress(ctx, tx, "recording_packages", id, value)
	})
}

func (s *RecordingSourceStore) UpdateSourceProcessingProgress(ctx context.Context, id, claimID string, value assets.RecordingProcessingProgress) error {
	if err := assets.ValidateRecordingProcessingProgress(value); err != nil {
		return err
	}
	return s.sourceClaimTransaction(ctx, id, claimID, func(tx *sql.Tx, _ assets.RecordingSource) error {
		return updateRecordingProgress(ctx, tx, "recording_sources", id, value)
	})
}

// The caller already holds the live slot and owner-row locks. Table names are
// private constants from the two fenced methods, never caller input.
func updateRecordingProgress(ctx context.Context, tx *sql.Tx, table, id string, value assets.RecordingProcessingProgress) error {
	var data []byte
	if err := tx.QueryRowContext(ctx, "SELECT processing_progress FROM "+table+" WHERE id=$1", id).Scan(&data); err != nil {
		return err
	}
	if len(data) == 0 {
		return assets.ErrConflict
	}
	var old assets.RecordingProcessingProgress
	if err := json.Unmarshal(data, &old); err != nil {
		return err
	}
	if value.Attempt != old.Attempt || !value.AttemptStartedAt.Equal(old.AttemptStartedAt) || value.PhaseStartedAt.Before(old.PhaseStartedAt) || value.LastProgressAt.Before(old.LastProgressAt) {
		return assets.ErrConflict
	}
	for _, pair := range [][2]*int64{{value.ObjectsVerified, old.ObjectsVerified}, {value.SegmentsVerified, old.SegmentsVerified}, {value.BytesVerified, old.BytesVerified}} {
		if pair[1] != nil && (pair[0] == nil || *pair[0] < *pair[1]) {
			return assets.ErrConflict
		}
	}
	if value.HeartbeatAt.Before(old.HeartbeatAt) {
		value.HeartbeatAt = old.HeartbeatAt
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE "+table+" SET processing_progress=$2 WHERE id=$1", id, data)
	return err
}
