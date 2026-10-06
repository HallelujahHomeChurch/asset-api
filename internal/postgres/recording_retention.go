package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/auditclient"
	"hhc/asset-api/internal/auditoutbox"
)

const retentionLock = `hashtextextended('recording-retention-policy',0)`

// Always take policy before slot/package/cover locks. Contention is retryable,
// never a pool full of waiting connections or an unbounded management request.
func beginRecordingPolicyTx(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	var locked bool
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock_shared(`+retentionLock+`)`).Scan(&locked); err != nil || !locked {
		tx.Rollback()
		if err == nil {
			err = assets.ErrConflict
		}
		return nil, err
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM recording_retention_policy WHERE singleton`).Scan(&revision); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (s *RecordingPackageStore) GetRetentionPolicy(ctx context.Context) (assets.RecordingRetentionPolicy, error) {
	var p assets.RecordingRetentionPolicy
	err := s.db.QueryRowContext(ctx, `SELECT retention_days,revision,activated_at FROM recording_retention_policy WHERE singleton`).Scan(&p.RetentionDays, &p.Revision, &p.ActivatedAt)
	return p, err
}

func retentionActor(ctx context.Context) (auditclient.Provenance, error) {
	p, ok := auditclient.ProvenanceFromContext(ctx)
	if !ok || p.ActorType != "user" || !auditclient.ValidUserRequest(p.ActorID, p.RequestID) {
		return p, assets.ErrForbidden
	}
	return p, nil
}

// One consistent, bounded snapshot lets CMS filter/sort before pagination without
// one provider request per recording or a second independently editable policy.
func (s *RecordingPackageStore) RecordingLifecycle(ctx context.Context, bindings []assets.RecordingLifecycleBinding, sources []assets.RecordingSourceLifecycleBinding) (assets.RecordingLifecycleSnapshot, error) {
	v := assets.RecordingLifecycleSnapshot{Items: []assets.RecordingPackageStatus{}}
	if len(bindings)+len(sources) > 1000 {
		return v, assets.ErrInvalidInput
	}
	ids := make([]string, 0, len(bindings))
	wanted := make(map[string]string, len(bindings))
	for _, b := range bindings {
		if b.PackageID == "" || b.RecordingID == "" || len(b.PackageID) > 80 || len(b.RecordingID) > 80 || wanted[b.PackageID] != "" {
			return v, assets.ErrInvalidInput
		}
		wanted[b.PackageID] = b.RecordingID
		ids = append(ids, b.PackageID)
	}
	sourceIDs := make([]string, 0, len(sources))
	wantedSources := make(map[string]string, len(sources))
	for _, b := range sources {
		if b.SourceID == "" || b.RecordingID == "" || len(b.SourceID) > 80 || len(b.RecordingID) > 80 || wantedSources[b.SourceID] != "" {
			return v, assets.ErrInvalidInput
		}
		wantedSources[b.SourceID] = b.RecordingID
		sourceIDs = append(sourceIDs, b.SourceID)
	}
	tx, err := beginRecordingPolicyTx(ctx, s.db)
	if err != nil {
		return v, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT retention_days,revision,activated_at FROM recording_retention_policy WHERE singleton`).Scan(&v.Policy.RetentionDays, &v.Policy.Revision, &v.Policy.ActivatedAt)
	if err != nil {
		return v, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,recording_id,state,size_bytes,expires_at,completed_at,ready_at,media_expires_at,retention_revision,processing_progress FROM recording_packages WHERE id=ANY($1) AND owner_service='hhc-web-api'`, ids)
	if err != nil {
		return v, err
	}
	for rows.Next() {
		var p assets.RecordingPackageStatus
		var progress []byte
		if err := rows.Scan(&p.PackageID, &p.RecordingID, &p.State, &p.SizeBytes, &p.ExpiresAt, &p.UploadedAt, &p.ReadyAt, &p.MediaExpiresAt, &p.RetentionRevision, &progress); err != nil {
			rows.Close()
			return v, err
		}
		p.ProcessingProgress, err = decodeRecordingProgress(progress)
		if err != nil {
			rows.Close()
			return v, err
		}
		if wanted[p.PackageID] != p.RecordingID {
			rows.Close()
			return v, assets.ErrForbidden
		}
		p.SessionID = p.PackageID
		p.ConfirmedObjects = []string{}
		v.Items = append(v.Items, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return v, err
	}
	if len(v.Items) != len(bindings) {
		return v, assets.ErrNotFound
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,recording_id,state,processing_progress FROM recording_sources WHERE id=ANY($1) AND owner_service='hhc-web-api'`, sourceIDs)
	if err != nil {
		return v, err
	}
	for rows.Next() {
		var p assets.RecordingSourceLifecycleStatus
		var progress []byte
		if err := rows.Scan(&p.SourceID, &p.RecordingID, &p.State, &progress); err != nil {
			rows.Close()
			return v, err
		}
		if wantedSources[p.SourceID] != p.RecordingID {
			rows.Close()
			return v, assets.ErrForbidden
		}
		p.ProcessingProgress, err = decodeRecordingProgress(progress)
		if err != nil {
			rows.Close()
			return v, err
		}
		v.SourceItems = append(v.SourceItems, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return v, err
	}
	if len(v.SourceItems) != len(sources) {
		return v, assets.ErrNotFound
	}
	return v, tx.Commit()
}

func (s *RecordingPackageStore) PreviewRetentionPolicy(ctx context.Context, days int) (assets.RecordingRetentionPreview, error) {
	var p assets.RecordingRetentionPreview
	if days < 1 || days > 365 {
		return p, assets.ErrInvalidInput
	}
	actor, err := retentionActor(ctx)
	if err != nil {
		return p, err
	}
	tx, err := beginRecordingPolicyTx(ctx, s.db)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	p.PreviewID, p.RetentionDays = newStoreID(), days
	err = tx.QueryRowContext(ctx, `SELECT revision,clock_timestamp() FROM recording_retention_policy WHERE singleton`).Scan(&p.Revision, &p.EvaluatedAt)
	if err != nil {
		return p, err
	}
	// Missing immutable completion is an error, never a guessed fresh timestamp.
	var missing bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recording_packages WHERE state='ready' AND completed_at IS NULL)`).Scan(&missing); err != nil {
		return p, err
	}
	if missing {
		return p, assets.ErrConflict
	}
	// Source bytes estimate pending output, not its eventual encoded size. A
	// source with a materialized package is counted through that package only.
	err = tx.QueryRowContext(ctx, `WITH affected AS (
 SELECT recording_id,size_bytes FROM recording_packages
 WHERE state IN ('freezing','validating','ready') AND completed_at+($1*interval '24 hours')<=$2
 UNION ALL
 SELECT recording_id,size_bytes FROM recording_sources
 WHERE package_id IS NULL AND completed_at+($1*interval '24 hours')<=$2
 AND (state IN ('finalizing','queued','processing') OR (state='failed' AND processing_error='retry' AND processing_attempts<3 AND retry_until>$2))
), recordings AS (SELECT recording_id,max(size_bytes) AS size_bytes FROM affected GROUP BY recording_id)
 SELECT count(*),COALESCE(sum(size_bytes),0) FROM recordings`, days, p.EvaluatedAt).Scan(&p.AffectedCount, &p.AffectedBytes)
	if err != nil {
		return p, err
	}
	// Unconfirmed previews contain no durable business receipt and expire quickly.
	if _, err = tx.ExecContext(ctx, `DELETE FROM recording_retention_previews WHERE result IS NULL AND evaluated_at<now()-interval '5 minutes'`); err != nil {
		return p, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO recording_retention_previews(id,actor_id,retention_days,revision,evaluated_at,affected_count,affected_bytes) VALUES($1,$2,$3,$4,$5,$6,$7)`, p.PreviewID, actor.ActorID, days, p.Revision, p.EvaluatedAt, p.AffectedCount, p.AffectedBytes)
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}

func (s *RecordingPackageStore) UpdateRetentionPolicy(ctx context.Context, in assets.UpdateRecordingRetentionInput) (assets.RecordingRetentionPolicy, error) {
	var p assets.RecordingRetentionPolicy
	if in.RetentionDays < 1 || in.RetentionDays > 365 || in.ExpectedRevision < 1 || in.PreviewID == "" || strings.TrimSpace(in.IdempotencyKey) == "" || len(in.IdempotencyKey) > 128 {
		return p, assets.ErrInvalidInput
	}
	actor, err := retentionActor(ctx)
	if err != nil {
		return p, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	var locked bool
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(`+retentionLock+`)`).Scan(&locked); err != nil {
		return p, err
	}
	if !locked {
		return p, assets.ErrConflict
	}
	var originalID string
	var saved []byte
	err = tx.QueryRowContext(ctx, `SELECT id,result FROM recording_retention_previews WHERE actor_id=$1 AND idempotency_key=$2`, actor.ActorID, in.IdempotencyKey).Scan(&originalID, &saved)
	if err == nil {
		if originalID != in.PreviewID || json.Unmarshal(saved, &p) != nil || p.RetentionDays != in.RetentionDays || p.Revision != in.ExpectedRevision+1 {
			return p, assets.ErrConflict
		}
		return p, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	err = tx.QueryRowContext(ctx, `SELECT retention_days,revision,activated_at FROM recording_retention_policy WHERE singleton FOR UPDATE`).Scan(&p.RetentionDays, &p.Revision, &p.ActivatedAt)
	if err != nil {
		return p, err
	}
	if p.Revision != in.ExpectedRevision {
		return p, assets.ErrConflict
	}
	var confirmed bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recording_retention_previews WHERE id=$1 AND actor_id=$2 AND retention_days=$3 AND revision=$4 AND evaluated_at>clock_timestamp()-interval '5 minutes' AND result IS NULL)`, in.PreviewID, actor.ActorID, in.RetentionDays, in.ExpectedRevision).Scan(&confirmed)
	if err != nil {
		return p, err
	}
	if !confirmed {
		return p, assets.ErrConflict
	}
	var missing bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recording_packages WHERE state='ready' AND completed_at IS NULL)`).Scan(&missing); err != nil {
		return p, err
	}
	if missing {
		return p, assets.ErrConflict
	}
	previousDays := p.RetentionDays
	err = tx.QueryRowContext(ctx, `UPDATE recording_retention_policy SET retention_days=$1,revision=revision+1,activated_at=COALESCE(activated_at,clock_timestamp()) WHERE singleton RETURNING retention_days,revision,activated_at`, in.RetentionDays).Scan(&p.RetentionDays, &p.Revision, &p.ActivatedAt)
	if err != nil {
		return p, err
	}
	// Preserve already-issued grants even when the new expiry is far in the past.
	_, err = tx.ExecContext(ctx, `UPDATE recording_packages SET media_expires_at=completed_at+($1*interval '24 hours'),retention_revision=$2,grant_cleanup_after=GREATEST(grant_cleanup_after,clock_timestamp()+interval '1 hour'),cleanup_after=clock_timestamp() WHERE state='ready'`, p.RetentionDays, p.Revision)
	if err != nil {
		return p, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE recording_covers c SET expires_at=p.media_expires_at,cleanup_after=clock_timestamp() FROM recording_packages p WHERE c.package_id=p.id AND p.state='ready' AND c.state<>'expired'`)
	if err != nil {
		return p, err
	}
	now := time.Now().UTC()
	event, err := auditclient.NewEvent("asset.recording.retention.update", "singleton", actor.ActorType, actor.ActorID, actor.RequestID, now, auditclient.Metadata{"previousDays": previousDays, "retentionDays": p.RetentionDays, "revision": p.Revision})
	if err != nil {
		return p, assets.ErrAuditUnavailable
	}
	if err := auditoutbox.New(s.db).EnqueueTx(ctx, tx, event, now); err != nil {
		return p, assets.ErrAuditUnavailable
	}
	saved, err = json.Marshal(p)
	if err != nil {
		return p, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE recording_retention_previews SET idempotency_key=$2,result=$3 WHERE id=$1`, in.PreviewID, in.IdempotencyKey, saved)
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}
