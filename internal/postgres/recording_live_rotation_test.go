package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
)

func TestLiveAttemptSweepRevisitsEveryPurgedAttempt(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingCaptureStore(db)
	id := strings.Repeat("d", 32)
	now := time.Now()
	_, err := store.CreateCapture(ctx, assets.RecordingCapture{ID: id, ActorID: "actor", RecordingID: "recording", CreateKey: "rotation", State: "uploading", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Receipts: map[string]assets.RecordingCaptureStoredReceipt{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE recording_captures SET state='failed',created_at=now()-interval '10 days',expires_at=now()-interval '9 days',terminal_at=now()-interval '8 days' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO recording_live_attempts(claim_id,capture_id,sequence,created_at,purged) SELECT lpad(n::text,32,'0'),$1,n,now(),true FROM generate_series(1,211) n`, id); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for range 3 {
		err = store.cleanupLive(ctx, id, func(_ context.Context, keys []string) error {
			for _, key := range keys {
				if strings.HasPrefix(key, "recordings/packages/") {
					seen[strings.Split(key, "/")[4]] = true
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 211 {
		t.Fatalf("only %d/211 purged claims received another sweep; late objects off page one survive", len(seen))
	}
	if !seen[fmt.Sprintf("%032d", 211)] {
		t.Fatal("missing final claim")
	}
}
