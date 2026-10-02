package postgres

import (
	"context"
	"database/sql"
	"time"
)

type RecordingHealthSnapshot struct {
	PackageCleanupOverdue int64
	SourceCleanupOverdue  int64
	Waiting               int64
	ActiveSlots           int64
}

// RecordingHealth exposes only aggregate counts. Cleanup eligibility is shared
// with the reconcilers; live uploads and fenced processing are not overdue.
func RecordingHealth(ctx context.Context, db *sql.DB) (RecordingHealthSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var result RecordingHealthSnapshot
	err := db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM recording_packages p WHERE cleanup_after<clock_timestamp()-interval '1 hour' AND `+packageCleanupEligible+`),
 (SELECT count(*) FROM recording_sources s WHERE cleanup_after<clock_timestamp()-interval '1 hour' AND `+sourceCleanupEligible+`),
 (SELECT count(*) FROM recording_sources WHERE state IN ('finalizing','queued') AND (claimed_until IS NULL OR claimed_until<=clock_timestamp())) +
 (SELECT count(*) FROM recording_packages WHERE state IN ('freezing','validating') AND (claimed_until IS NULL OR claimed_until<=clock_timestamp())),
 (SELECT count(*) FROM recording_processing_slots WHERE leased_until>clock_timestamp())`).Scan(&result.PackageCleanupOverdue, &result.SourceCleanupOverdue, &result.Waiting, &result.ActiveSlots)
	return result, err
}
