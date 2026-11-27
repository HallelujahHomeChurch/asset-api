package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
)

const liveRepairCandidate = `SELECT r.capture_id,r.sequence,r.rendition FROM recording_live_repairs r
 JOIN recording_captures c ON c.id=r.capture_id
 WHERE r.next_attempt_at<=now() AND c.terminal_at IS NULL AND c.expires_at>now()
 AND c.state IN ('uploading','freezing','validating','ready') AND NOT (` + capturePackageTerminal + `)
 AND EXISTS (SELECT 1 FROM recording_capture_objects o WHERE o.capture_id=c.id AND o.path=r.rendition||'/seg-'||lpad(r.sequence::text,6,'0')||'.m4s' AND o.state IN ('queued','verified'))
 ORDER BY r.next_attempt_at LIMIT 1`

type LiveRepair struct {
	RecordingLiveClaim
	Rendition string
}

// Repair uses a background slot. It never owns the broadcast's input or live lease.
func (s *RecordingCaptureStore) ClaimLiveRepair(ctx context.Context) (LiveRepair, error) {
	var r LiveRepair
	var id string
	err := s.db.QueryRowContext(ctx, liveRepairCandidate).Scan(&id, &r.Sequence, &r.Rendition)
	if errors.Is(err, sql.ErrNoRows) {
		return r, assets.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	err = s.liveTransaction(ctx, id, "", func(tx *sql.Tx, c assets.RecordingCapture, snap RecordingLiveSnapshot) error {
		var slot int
		err := tx.QueryRowContext(ctx, claimBackgroundSlot).Scan(&slot)
		if errors.Is(err, sql.ErrNoRows) {
			return assets.ErrNotFound
		}
		if err != nil {
			return err
		}
		r.ClaimID = newStoreID()
		r.Capture = c
		r.Snapshot = snap
		if r.Sequence >= len(snap.Segments) || !snap.Segments[r.Sequence].Renditions[r.Rendition].Gap {
			return assets.ErrNotFound
		}
		result, err := tx.ExecContext(ctx, `UPDATE recording_live_repairs SET claim_id=$4,next_attempt_at=now()+interval '2 minutes' WHERE capture_id=$1 AND sequence=$2 AND rendition=$3 AND next_attempt_at<=now()`, id, r.Sequence, r.Rendition, r.ClaimID)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return assets.ErrNotFound
		}
		for _, path := range []string{r.Rendition + "/init.mp4", fmt.Sprintf("%s/seg-%06d.m4s", r.Rendition, r.Sequence)} {
			var o assets.RecordingPackageObject
			o.Path = path
			if err := tx.QueryRowContext(ctx, `SELECT size_bytes,sha256 FROM recording_capture_objects WHERE capture_id=$1 AND path=$2 AND state IN ('queued','verified')`, id, path).Scan(&o.SizeBytes, &o.SHA256); err != nil {
				return err
			}
			r.Objects = append(r.Objects, o)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO recording_live_attempts(claim_id,capture_id,sequence) VALUES($1,$2,$3)`, r.ClaimID, id, r.Sequence); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=$1,claim_id=$2,leased_until=now()+interval '2 minutes' WHERE slot=$3`, "repair:"+id, r.ClaimID, slot)
		return err
	})
	return r, err
}

func (s *RecordingCaptureStore) FinishLiveRepair(ctx context.Context, r LiveRepair, fragment *assets.RecordingLiveFragment) error {
	return s.liveTransaction(ctx, r.Capture.ID, "", func(tx *sql.Tx, c assets.RecordingCapture, snap RecordingLiveSnapshot) error {
		var owned bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recording_live_repairs WHERE capture_id=$1 AND sequence=$2 AND rendition=$3 AND claim_id=$4 AND next_attempt_at>clock_timestamp()) AND EXISTS(SELECT 1 FROM recording_processing_slots WHERE claim_id=$4 AND leased_until>clock_timestamp())`, c.ID, r.Sequence, r.Rendition, r.ClaimID).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return assets.ErrConflict
		}
		if fragment != nil && snap.Revision == snap.PublishedRevision && snap.Segments[r.Sequence].Renditions[r.Rendition].Gap {
			fragment.WasGap = true
			raw, err := json.Marshal(fragment)
			if err != nil {
				return err
			}
			// Active publishers may still be composing an older snapshot. Leave it intact
			// until their lease ends. The durable repair remains eligible for another turn.
			result, err := tx.ExecContext(ctx, `UPDATE recording_live SET segments=jsonb_set(segments,ARRAY[$2,'renditions',$3]::text[],$4::jsonb),revision=revision+1
      WHERE capture_id=$1 AND (claim_id IS NULL OR claimed_until<=clock_timestamp())`, c.ID, fmt.Sprint(r.Sequence), r.Rendition, raw)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n == 1 {
				if _, err = tx.ExecContext(ctx, `UPDATE recording_capture_objects SET state='verified' WHERE capture_id=$1 AND path=$2 AND sha256=$3 AND size_bytes=$4`, c.ID, r.Objects[1].Path, r.Objects[1].SHA256, r.Objects[1].SizeBytes); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, `DELETE FROM recording_live_repairs WHERE claim_id=$1`, r.ClaimID); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recording_live_repairs SET claim_id=NULL,next_attempt_at=now()+interval '30 seconds' WHERE claim_id=$1`, r.ClaimID); err != nil {
			return err
		}
		return releaseLiveClaim(ctx, tx, r.RecordingLiveClaim)
	})
}
