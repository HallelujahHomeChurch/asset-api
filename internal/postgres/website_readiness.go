package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"hhc/asset-api/internal/assets"
)

func (s *Store) WebsiteContentReadiness(ctx context.Context, refs []assets.WebsiteContentRef) ([]assets.WebsiteContentDecision, error) {
	if err := assets.ValidateWebsiteContentRefs(refs); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT x.type,x.id,x."resourceId",COALESCE(x.locale,''),
 CASE WHEN x.type='bulletin_pdf' THEN a.id IS NOT NULL ELSE c.id IS NOT NULL AND c.terminal_at IS NULL AND c.state IN ('uploading','freezing','validating','ready') AND c.expires_at>clock_timestamp() AND NOT (`+capturePackageTerminal+`) AND NOT EXISTS(SELECT 1 FROM recording_deletions d WHERE d.recording_id=c.recording_id) AND l.capture_id IS NOT NULL END,
 c.state,c.created_at,c.expires_at,c.terminal_at,l.published_revision,l.published_sequence,l.published_media_end,l.last_advanced_at,l.ended_at,l.ended AND l.published_revision=l.revision
 FROM jsonb_to_recordset($1::jsonb) AS x(type text,id text,"resourceId" text,locale text)
 LEFT JOIN assets a ON x.type='bulletin_pdf' AND a.id=x.id AND a.owner_service='hhc-web-api' AND a.namespace='cms.weekly.pdf' AND a.owner_type='bulletin_issue' AND a.owner_id=x."resourceId" AND a.locale=x.locale AND a.upload_status='completed' AND a.scan_status='clean' AND a.processing_status IN ('ready','not_required') AND a.visibility='private' AND a.deleted_at IS NULL AND a.purged_at IS NULL
 LEFT JOIN recording_captures c ON x.type='recording_live' AND c.id=x.id AND c.recording_id=x."resourceId"
 LEFT JOIN recording_live l ON l.capture_id=c.id`, raw)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]assets.WebsiteContentDecision, 0, len(refs))
	for rows.Next() {
		var d assets.WebsiteContentDecision
		var state sql.NullString
		var created, expires, terminal, advanced, endedAt sql.NullTime
		var revision, last sql.NullInt64
		var media sql.NullFloat64
		var ended sql.NullBool
		if err = rows.Scan(&d.Ref.Type, &d.Ref.ID, &d.Ref.ResourceID, &d.Ref.Locale, &d.Ready, &state, &created, &expires, &terminal, &revision, &last, &media, &advanced, &endedAt, &ended); err != nil {
			return nil, err
		}
		if d.Ref.Type == "recording_live" && d.Ready {
			v := assets.WebsiteLiveReadiness{State: state.String, CreatedAt: created.Time, ExpiresAt: expires.Time, Progress: assets.RecordingLiveProgress{Revision: revision.Int64, LastSequence: int(last.Int64), MediaEndSeconds: media.Float64, Ended: ended.Bool}}
			if terminal.Valid {
				v.TerminalAt = &terminal.Time
			}
			if advanced.Valid {
				v.Progress.LastAdvancedAt = &advanced.Time
			}
			if endedAt.Valid {
				v.Progress.EndedAt = &endedAt.Time
			}
			d.Live = &v
		}
		items = append(items, d)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	ordered := make([]assets.WebsiteContentDecision, len(refs))
	byRef := make(map[assets.WebsiteContentRef]assets.WebsiteContentDecision, len(items))
	for _, item := range items {
		byRef[item.Ref] = item
	}
	for i, ref := range refs {
		ordered[i] = byRef[ref]
	}
	return ordered, tx.Commit()
}
