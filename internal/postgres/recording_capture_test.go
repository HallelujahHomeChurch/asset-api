package postgres

import (
	"context"
	"database/sql"
	"errors"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"testing"
	"time"
)

type abortReadyBarrierRepository struct {
	assets.RecordingCaptureRepository
	staleRead      chan struct{}
	readyCommitted chan struct{}
}

func (r abortReadyBarrierRepository) UpdateCapture(ctx context.Context, id string, fn func(*assets.RecordingCapture) error) (assets.RecordingCapture, error) {
	return r.RecordingCaptureRepository.UpdateCapture(ctx, id, func(c *assets.RecordingCapture) error {
		if err := fn(c); err != nil {
			return err
		}
		close(r.staleRead)
		select {
		case <-r.readyCommitted:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}
func TestCaptureAbortRejectsReadyCommittedAfterStaleRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := isolatedIntegrationDB(t)
	db.SetMaxOpenConns(2)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	packages := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "package-a", "22222222-2222-4222-8222-222222222222", "11111111-1111-4111-8111-111111111111")
	if err := packages.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := packages.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	claim, err := packages.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO recording_captures(id,actor_id,recording_id,create_key,state,created_at,expires_at,package_id) VALUES($1,$2,$3,'create-a','validating',$4,$5,$1)`, p.ID, p.ActorID, p.RecordingID, p.CreatedAt, p.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO recording_live(capture_id) SELECT id FROM recording_captures ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	barrier := abortReadyBarrierRepository{NewRecordingCaptureStore(db), make(chan struct{}), make(chan struct{})}
	workerLocked := make(chan struct{})
	workerResult := make(chan error, 1)
	go func() {
		// The actual worker transaction helper locks slot then package. Pause it
		// while Abort reads validating on the second connection, then commit ready.
		err := packages.packageClaimTransaction(ctx, p.ID, claim.ClaimID, func(tx *sql.Tx) error {
			close(workerLocked)
			select {
			case <-barrier.staleRead:
			case <-ctx.Done():
				return ctx.Err()
			}
			if _, err := tx.ExecContext(ctx, `UPDATE recording_packages SET state='ready',final_prefix=$2,ready_at=now(),media_expires_at=now()+interval '30 days',claim_id=NULL,claimed_until=NULL WHERE id=$1`, p.ID, "recordings/packages/package-a/final/"+claim.ClaimID+"/"); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=NULL,claim_id=NULL,leased_until=NULL WHERE claim_id=$1`, claim.ClaimID)
			return err
		})
		workerResult <- err
		close(barrier.readyCommitted)
	}()
	select {
	case <-workerLocked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	svc := assets.NewRecordingCaptureService(barrier, captureObjectStore{}, time.Now)
	_, abortErr := svc.Abort(ctx, p.ID, p.ActorID, "abort-race", "user_abort")
	if err := <-workerResult; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(abortErr, assets.ErrConflict) {
		t.Fatalf("ready won but abort accepted: %v", abortErr)
	}
	var captureState, packageState string
	var receiptExists bool
	if err = db.QueryRowContext(ctx, `SELECT c.state,p.state,c.receipts ? 'abort-race' FROM recording_captures c JOIN recording_packages p ON p.id=c.package_id WHERE c.id=$1`, p.ID).Scan(&captureState, &packageState, &receiptExists); err != nil {
		t.Fatal(err)
	}
	if packageState != "ready" || captureState == "aborted" || receiptExists {
		t.Fatalf("abort committed after ready: capture=%s package=%s receipt=%v", captureState, packageState, receiptExists)
	}
}
