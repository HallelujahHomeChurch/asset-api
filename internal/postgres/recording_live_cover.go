package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"regexp"
	"strings"
	"time"
)

type RecordingLiveCoverStore struct{ db *sql.DB }

func NewRecordingLiveCoverStore(db *sql.DB) *RecordingLiveCoverStore {
	return &RecordingLiveCoverStore{db}
}

func (s *RecordingLiveCoverStore) AutoMedia(ctx context.Context, c assets.RecordingLiveCover) (float64, []assets.RecordingPackageObject, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT l.segments->COALESCE(b.start_sequence,0) FROM recording_live l LEFT JOIN recording_broadcast_ranges b ON b.capture_id=l.capture_id JOIN recording_live_covers c ON c.scope=l.capture_id WHERE c.id=$1 AND c.claim_id=$2 AND c.state='processing' AND c.claimed_until>now() AND l.published_sequence>=COALESCE(b.start_sequence,0) AND (b.capture_id IS NULL OR b.start_sequence IS NOT NULL AND NOT b.revoked) AND `+liveCoverAlive, c.ID, c.ClaimID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, assets.ErrConflict
	}
	if err != nil {
		return 0, nil, err
	}
	var segment assets.RecordingLiveSegment
	if err = json.Unmarshal(raw, &segment); err != nil {
		return 0, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path,size_bytes,sha256 FROM recording_capture_objects WHERE capture_id=$1 AND state='verified' AND path IN ('480p/init.mp4',$2) ORDER BY path`, c.Scope, fmt.Sprintf("480p/seg-%06d.m4s", segment.Sequence))
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var media []assets.RecordingPackageObject
	for rows.Next() {
		var object assets.RecordingPackageObject
		if err = rows.Scan(&object.Path, &object.SizeBytes, &object.SHA256); err != nil {
			return 0, nil, err
		}
		media = append(media, object)
	}
	return segment.Renditions["480p"].End - segment.Renditions["480p"].Start, media, rows.Err()
}

// Promotion queues a new immutable VOD cover; existing candidates are not mutated.
func (s *RecordingLiveCoverStore) Promote(ctx context.Context, id, capture, recording string) (assets.RecordingCover, error) {
	source, err := s.Get(ctx, id, capture, recording)
	if err != nil {
		return assets.RecordingCover{}, err
	}
	return s.promoteSource(ctx, source, capture, recording)
}
func (s *RecordingLiveCoverStore) promoteSource(ctx context.Context, source assets.RecordingLiveCover, capture, recording string) (assets.RecordingCover, error) {
	id := source.ID
	if source.State != "ready" {
		return assets.RecordingCover{}, assets.ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.RecordingCover{}, err
	}
	defer tx.Rollback()
	if err = checkRecordingNotDeleted(ctx, tx, recording); err != nil {
		return assets.RecordingCover{}, err
	}
	var expiry time.Time
	err = tx.QueryRowContext(ctx, `SELECT media_expires_at FROM recording_packages WHERE id=$1 AND recording_id=$2 AND state='ready' AND owner_service='hhc-web-api' AND media_expires_at>now() FOR SHARE`, capture, recording).Scan(&expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.RecordingCover{}, assets.ErrNotFound
	}
	if err != nil {
		return assets.RecordingCover{}, err
	}
	kind := "custom"
	if source.Kind == "auto" {
		kind = "live-auto"
	}
	coverID := newStoreID()
	_, err = tx.ExecContext(ctx, `INSERT INTO recording_covers(id,package_id,recording_id,actor_id,idempotency_key,kind,mime,digest,state,expires_at,inherited_cover_id) VALUES($1,$2,$3,'live-cover-promotion',$4,$5,'image/jpeg',$6,'pending',$7,$8) ON CONFLICT(recording_id,actor_id,idempotency_key) DO NOTHING`, coverID, capture, recording, "promote:"+id, kind, source.OutputDigest, expiry, id)
	if err != nil {
		return assets.RecordingCover{}, err
	}
	c, err := scanCover(tx.QueryRowContext(ctx, `SELECT `+coverColumns+` FROM recording_covers c WHERE recording_id=$1 AND actor_id='live-cover-promotion' AND idempotency_key=$2 AND package_id=$3`, recording, "promote:"+id, capture))
	if err != nil {
		return c, err
	}
	reference := "vod:" + c.ID
	_, err = tx.ExecContext(ctx, `INSERT INTO recording_live_cover_references(reference_id,cover_id,scope,recording_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, reference, id, capture, recording)
	if err != nil {
		return c, err
	}
	return c, tx.Commit()
}
func (s *RecordingLiveCoverStore) Timeline(ctx context.Context, capture string) ([]assets.RecordingLiveSegment, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT CASE WHEN b.capture_id IS NULL THEN l.segments WHEN b.start_sequence IS NOT NULL AND b.end_sequence_exclusive IS NOT NULL AND NOT b.revoked AND l.published_sequence>=b.end_sequence_exclusive-1 THEN (SELECT jsonb_agg(v ORDER BY ord) FROM jsonb_array_elements(l.segments) WITH ORDINALITY AS x(v,ord) WHERE ord>b.start_sequence AND ord<=b.end_sequence_exclusive) ELSE NULL END FROM recording_live l LEFT JOIN recording_broadcast_ranges b ON b.capture_id=l.capture_id WHERE l.capture_id=$1`, capture).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, assets.ErrConflict
	}
	var result []assets.RecordingLiveSegment
	err = json.Unmarshal(raw, &result)
	return result, err
}

var broadcastRecordingUUID = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
var liveCoverScope = regexp.MustCompile(`^[a-f0-9]{32}$`)

const liveCoverColumns = `c.id,c.scope,c.recording_id,c.kind,c.state,c.mime,c.digest,c.output_digest,COALESCE(c.claim_id,''),c.output_attempt,c.created_at`

// Ready derivatives follow package retention, not the closed intake deadline.
const liveCoverCaptureAlive = `cap.terminal_at IS NULL AND cap.state IN ('uploading','freezing','validating','ready') AND (cap.state<>'ready' AND cap.expires_at>now() OR cap.state='ready' AND EXISTS(SELECT 1 FROM recording_packages p WHERE p.id=cap.package_id AND p.recording_id=cap.recording_id AND p.owner_service='hhc-web-api' AND p.state='ready' AND p.media_expires_at>now()))`
const liveCoverAlive = `(c.scope='defaults' OR c.scope='broadcast-'||c.recording_id OR EXISTS(SELECT 1 FROM recording_captures cap WHERE cap.id=c.scope AND cap.recording_id=c.recording_id AND ` + liveCoverCaptureAlive + `)) AND NOT EXISTS(SELECT 1 FROM recording_deletions d WHERE d.recording_id=c.recording_id)`

func scanLiveCover(row coverScanner) (assets.RecordingLiveCover, error) {
	var c assets.RecordingLiveCover
	err := row.Scan(&c.ID, &c.Scope, &c.RecordingID, &c.Kind, &c.State, &c.MIME, &c.Digest, &c.OutputDigest, &c.ClaimID, &c.OutputAttempt, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = assets.ErrNotFound
	}
	return c, err
}
func checkLiveCoverOwner(ctx context.Context, tx *sql.Tx, scope, recording string) error {
	if scope == "defaults" {
		if recording != "" {
			return assets.ErrInvalidInput
		}
		return nil
	}
	if strings.HasPrefix(scope, "broadcast-") {
		if !broadcastRecordingUUID.MatchString(recording) || scope != "broadcast-"+recording {
			return assets.ErrInvalidInput
		}
		return checkRecordingNotDeleted(ctx, tx, recording)
	}
	if !liveCoverScope.MatchString(scope) || recording == "" {
		return assets.ErrInvalidInput
	}
	if err := checkRecordingNotDeleted(ctx, tx, recording); err != nil {
		return err
	}
	var found string
	err := tx.QueryRowContext(ctx, `SELECT cap.id FROM recording_captures cap WHERE cap.id=$1 AND cap.recording_id=$2 AND `+liveCoverCaptureAlive, scope, recording).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrNotFound
	}
	return err
}
func (s *RecordingLiveCoverStore) Create(ctx context.Context, scope, recording, actor, key, mime, digest, kind string) (assets.RecordingLiveCover, error) {
	var empty assets.RecordingLiveCover
	if actor == "" || len(actor) > 160 || key == "" || len(key) > 160 || strings.ContainsAny(key, "\r\n") || (kind != "auto" && kind != "custom") || kind == "auto" && (scope == "defaults" || strings.HasPrefix(scope, "broadcast-")) {
		return empty, assets.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	if err = checkLiveCoverOwner(ctx, tx, scope, recording); err != nil {
		return empty, err
	}
	// Serialize quota and replay checks without locking capture rows after slots.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('live-cover:'||$1,0))`, scope); err != nil {
		return empty, err
	}
	c, err := scanLiveCover(tx.QueryRowContext(ctx, `SELECT `+liveCoverColumns+` FROM recording_live_covers c WHERE scope=$1 AND actor_id=$2 AND idempotency_key=$3`, scope, actor, key))
	if err == nil {
		if c.Digest != digest || c.MIME != mime || c.Kind != kind || c.RecordingID != recording {
			return empty, assets.ErrConflict
		}
		return c, tx.Commit()
	}
	if !errors.Is(err, assets.ErrNotFound) {
		return empty, err
	}
	if kind == "auto" {
		c, err = scanLiveCover(tx.QueryRowContext(ctx, `SELECT `+liveCoverColumns+` FROM recording_live_covers c WHERE scope=$1 AND kind='auto'`, scope))
		if err == nil {
			return c, tx.Commit()
		}
		if !errors.Is(err, assets.ErrNotFound) {
			return empty, err
		}
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM recording_live_covers WHERE scope=$1 AND kind='custom' AND created_at>now()-interval '24 hours'`, scope).Scan(&count); err != nil {
		return empty, err
	}
	if count >= 20 {
		return empty, assets.ErrConflict
	}
	id := newStoreID()
	state := "uploading"
	if kind == "auto" {
		state = "pending"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO recording_live_covers(id,scope,recording_id,actor_id,idempotency_key,kind,mime,digest,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, scope, recording, actor, key, kind, mime, digest, state); err != nil {
		return empty, err
	}
	c, err = scanLiveCover(tx.QueryRowContext(ctx, `SELECT `+liveCoverColumns+` FROM recording_live_covers c WHERE id=$1`, id))
	if err != nil {
		return empty, err
	}
	return c, tx.Commit()
}
func (s *RecordingLiveCoverStore) Queue(ctx context.Context, id string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE recording_live_covers c SET state='pending' WHERE id=$1 AND state='uploading' AND created_at>now()-interval '24 hours' AND `+liveCoverAlive, id)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 1 {
		return nil
	}
	var state string
	if err = s.db.QueryRowContext(ctx, `SELECT state FROM recording_live_covers WHERE id=$1`, id).Scan(&state); err == nil && (state == "pending" || state == "processing" || state == "ready") {
		return nil
	}
	return assets.ErrConflict
}
func (s *RecordingLiveCoverStore) Get(ctx context.Context, id, scope, recording string) (assets.RecordingLiveCover, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.RecordingLiveCover{}, err
	}
	defer tx.Rollback()
	if err = checkLiveCoverOwner(ctx, tx, scope, recording); err != nil {
		return assets.RecordingLiveCover{}, err
	}
	c, err := scanLiveCover(tx.QueryRowContext(ctx, `SELECT `+liveCoverColumns+` FROM recording_live_covers c WHERE id=$1 AND state<>'expired' AND (created_at>now()-interval '24 hours' OR EXISTS(SELECT 1 FROM recording_live_cover_references r WHERE r.cover_id=c.id AND r.released_at IS NULL)) AND (c.scope=$2 AND c.recording_id=$3 OR EXISTS(SELECT 1 FROM recording_live_cover_references r WHERE r.cover_id=c.id AND r.scope=$2 AND r.recording_id=$3 AND r.released_at IS NULL)) AND `+liveCoverAlive, id, scope, recording))
	if err != nil {
		return c, err
	}
	return c, tx.Commit()
}
func (s *RecordingLiveCoverStore) Retain(ctx context.Context, id, scope, recording, reference string) error {
	if reference == "" || len(reference) > 160 || strings.ContainsAny(reference, "\r\n") {
		return assets.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkLiveCoverOwner(ctx, tx, scope, recording); err != nil {
		return err
	}
	var found string
	err = tx.QueryRowContext(ctx, `SELECT c.id FROM recording_live_covers c WHERE id=$1 AND state='ready' AND (c.scope='defaults' OR c.scope=$2 AND c.recording_id=$3 OR c.scope='broadcast-'||$3 AND c.recording_id=$3) AND (created_at>now()-interval '24 hours' OR EXISTS(SELECT 1 FROM recording_live_cover_references r WHERE r.cover_id=c.id AND r.released_at IS NULL)) AND `+liveCoverAlive+` FOR UPDATE`, id, scope, recording).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO recording_live_cover_references(reference_id,cover_id,scope,recording_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, reference, id, scope, recording); err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, `SELECT cover_id FROM recording_live_cover_references WHERE reference_id=$1 AND scope=$2 AND recording_id=$3 AND released_at IS NULL`, reference, scope, recording).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) || err == nil && found != id {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *RecordingLiveCoverStore) Release(ctx context.Context, id, scope, reference string) error {
	if reference == "" || len(reference) > 160 || strings.ContainsAny(reference, "\r\n") {
		return assets.ErrInvalidInput
	}
	r, err := s.db.ExecContext(ctx, `INSERT INTO recording_live_cover_references(reference_id,cover_id,scope,recording_id,released_at) SELECT $1,c.id,$3,COALESCE((SELECT recording_id FROM recording_captures WHERE id=$3),''),now() FROM recording_live_covers c WHERE c.id=$2 ON CONFLICT(reference_id) DO UPDATE SET released_at=COALESCE(recording_live_cover_references.released_at,EXCLUDED.released_at) WHERE recording_live_cover_references.cover_id=EXCLUDED.cover_id AND recording_live_cover_references.scope=EXCLUDED.scope`, reference, id, scope)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return assets.ErrConflict
	}
	return nil
}
func (s *RecordingLiveCoverStore) Claim(ctx context.Context) (assets.RecordingLiveCover, error) {
	var empty assets.RecordingLiveCover
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	var slot int
	err = tx.QueryRowContext(ctx, `SELECT slot FROM recording_processing_slots WHERE leased_until IS NULL OR leased_until<=now() ORDER BY slot FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, assets.ErrNotFound
	}
	if err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_live_covers SET state='failed',claim_id=NULL,claimed_until=NULL WHERE state='processing' AND attempts=3 AND claimed_until<=now()`); err != nil {
		return empty, err
	}
	c, err := scanLiveCover(tx.QueryRowContext(ctx, `SELECT `+liveCoverColumns+` FROM recording_live_covers c WHERE state IN ('pending','processing') AND attempts<3 AND next_attempt_at<=now() AND (claimed_until IS NULL OR claimed_until<=now()) AND created_at>now()-interval '24 hours' AND `+liveCoverAlive+` AND (kind='custom' OR EXISTS(SELECT 1 FROM recording_live l LEFT JOIN recording_broadcast_ranges b ON b.capture_id=l.capture_id WHERE l.capture_id=c.scope AND l.published_sequence>=COALESCE(b.start_sequence,0) AND (b.capture_id IS NULL OR b.start_sequence IS NOT NULL AND NOT b.revoked))) ORDER BY created_at FOR UPDATE OF c SKIP LOCKED LIMIT 1`))
	if errors.Is(err, assets.ErrNotFound) {
		if err = tx.Commit(); err != nil {
			return empty, err
		}
		return empty, assets.ErrNotFound
	}
	if err != nil {
		return empty, err
	}
	c.ClaimID = newStoreID()
	if _, err = tx.ExecContext(ctx, `UPDATE recording_live_covers SET state='processing',attempts=attempts+1,claim_id=$2,claimed_until=now()+interval '1 minute' WHERE id=$1`, c.ID, c.ClaimID); err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=$2,claim_id=$3,leased_until=now()+interval '1 minute' WHERE slot=$1`, slot, c.ID, c.ClaimID); err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO recording_live_cover_attempts(claim_id,cover_id) VALUES($1,$2)`, c.ClaimID, c.ID); err != nil {
		return empty, err
	}
	return c, tx.Commit()
}
func (s *RecordingLiveCoverStore) Finish(ctx context.Context, c assets.RecordingLiveCover, digest string, ready bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var slot int
	err = tx.QueryRowContext(ctx, `SELECT slot FROM recording_processing_slots WHERE job_id=$1 AND claim_id=$2 AND leased_until>now() FOR UPDATE`, c.ID, c.ClaimID).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	r, err := tx.ExecContext(ctx, `UPDATE recording_live_covers c SET state=CASE WHEN $3 THEN 'ready' WHEN attempts=3 THEN 'failed' ELSE 'pending' END,output_attempt=CASE WHEN $3 THEN $2 ELSE '' END,output_digest=CASE WHEN $3 THEN $4 ELSE '' END,claim_id=NULL,claimed_until=NULL,next_attempt_at=now()+interval '5 minutes' WHERE id=$1 AND claim_id=$2 AND state='processing' AND claimed_until>now() AND `+liveCoverAlive, c.ID, c.ClaimID, ready, digest)
	if err = recordingTransitionResult(r, err); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=NULL,claim_id=NULL,leased_until=NULL WHERE slot=$1`, slot); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *RecordingLiveCoverStore) Reconcile(ctx context.Context, remove func(context.Context, []string) error) error {
	// Recover the source hold after worker interruption, exhausted retries or deletion.
	_, err := s.db.ExecContext(ctx, `UPDATE recording_live_cover_references r SET released_at=now() WHERE reference_id IN (SELECT f.reference_id FROM recording_live_cover_references f JOIN recording_covers c ON f.reference_id='vod:'||c.id AND f.cover_id=c.inherited_cover_id AND f.scope=c.package_id AND f.recording_id=c.recording_id JOIN recording_packages p ON p.id=c.package_id WHERE f.released_at IS NULL AND (c.state IN ('ready','failed','expired') OR c.expires_at<=now() OR p.media_expires_at<=now() OR EXISTS(SELECT 1 FROM recording_deletions d WHERE d.recording_id=c.recording_id)) ORDER BY f.reference_id LIMIT 100) AND r.released_at IS NULL`)
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+liveCoverColumns+` FROM recording_live_covers c WHERE cleanup_after<=now() AND (claimed_until IS NULL OR claimed_until<=now()-interval '3 minutes') AND ((created_at<=now()-interval '24 hours' AND NOT EXISTS(SELECT 1 FROM recording_live_cover_references r WHERE r.cover_id=c.id AND r.released_at IS NULL)) OR NOT (`+liveCoverAlive+`)) ORDER BY created_at LIMIT 20`)
	if err != nil {
		return err
	}
	var covers []assets.RecordingLiveCover
	for rows.Next() {
		c, e := scanLiveCover(rows)
		if e != nil {
			rows.Close()
			return e
		}
		covers = append(covers, c)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, c := range covers {
		r, e := s.db.ExecContext(ctx, `UPDATE recording_live_covers c SET state='expired' WHERE id=$1 AND (claimed_until IS NULL OR claimed_until<=now()-interval '3 minutes') AND ((created_at<=now()-interval '24 hours' AND NOT EXISTS(SELECT 1 FROM recording_live_cover_references r WHERE r.cover_id=c.id AND r.released_at IS NULL)) OR NOT (`+liveCoverAlive+`))`, c.ID)
		if e != nil {
			return e
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			continue
		}
		attempts, e := s.db.QueryContext(ctx, `SELECT claim_id FROM recording_live_cover_attempts WHERE cover_id=$1`, c.ID)
		if e != nil {
			return e
		}
		keys := []string{c.InputKey()}
		for attempts.Next() {
			var a string
			if e = attempts.Scan(&a); e != nil {
				attempts.Close()
				return e
			}
			keys = append(keys, "recordings/covers/"+c.Scope+"/"+a+"/custom.jpg")
		}
		e = errors.Join(attempts.Err(), attempts.Close())
		if e == nil {
			e = remove(ctx, keys)
		}
		_, updateErr := s.db.ExecContext(ctx, `UPDATE recording_live_covers SET cleanup_after=now()+interval '1 day' WHERE id=$1`, c.ID)
		err = errors.Join(err, e, updateErr)
	}
	return err
}

func (s *RecordingLiveCoverStore) PromoteTo(ctx context.Context, id, scope, sourceRecording, capture, recording string) (assets.RecordingCover, error) {
	if scope != "defaults" && scope != "broadcast-"+recording {
		return assets.RecordingCover{}, assets.ErrInvalidInput
	}
	source, err := s.Get(ctx, id, scope, sourceRecording)
	if err != nil {
		return assets.RecordingCover{}, err
	}
	if source.Kind != "custom" || source.State != "ready" || scope != "defaults" && sourceRecording != recording {
		return assets.RecordingCover{}, assets.ErrConflict
	}
	return s.promoteSource(ctx, source, capture, recording)
}
