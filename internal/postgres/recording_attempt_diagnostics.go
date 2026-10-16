package postgres

import (
	"context"
	"database/sql"
	"encoding/json"

	"hhc/asset-api/internal/assets"
)

func (s *RecordingPackageStore) UpdatePackageAttemptDiagnostics(ctx context.Context, id, claim string, value assets.RecordingAttemptDiagnostics) error {
	return s.packageClaimTransaction(ctx, id, claim, func(tx *sql.Tx) error {
		return updateAttemptDiagnostics(ctx, tx, "recording_package_attempts", claim, value)
	})
}

func (s *RecordingSourceStore) UpdateSourceAttemptDiagnostics(ctx context.Context, id, claim string, value assets.RecordingAttemptDiagnostics) error {
	return s.sourceClaimTransaction(ctx, id, claim, func(tx *sql.Tx, _ assets.RecordingSource) error {
		return updateAttemptDiagnostics(ctx, tx, "recording_source_attempts", claim, value)
	})
}

// Both callers hold the live owner/slot locks; table names are private constants.
func updateAttemptDiagnostics(ctx context.Context, tx *sql.Tx, table, claim string, value assets.RecordingAttemptDiagnostics) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE "+table+" SET diagnostics=$2 WHERE claim_id=$1 AND state='processing' AND (diagnostics IS NULL OR (diagnostics->>'observedAt')::timestamptz <= $3)", claim, data, value.ObservedAt)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return assets.ErrConflict
	}
	return nil
}
