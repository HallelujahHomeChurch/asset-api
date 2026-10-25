package postgres

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/httpapi"
	"hhc/asset-api/internal/migrations"
)

func TestPackageGrantUsesOnePoolConnection(t *testing.T) {
	for _, mode := range []string{"no-policy", "vod", "revoked", "authority-mismatch", "projection-pending", "broadcast-disabled"} {
		t.Run(mode, func(t *testing.T) { testPackageGrantPool(t, mode) })
	}
}

func testPackageGrantPool(t *testing.T, mode string) {
	db := isolatedIntegrationDB(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	objects := &broadcastMemoryObjects{values: map[string][]byte{}}
	packages := NewRecordingPackageStore(db).WithBroadcastObjects(objects)
	p := testRecordingPackage(t, "package-grant-pool", "actor", "11111111-1111-4111-8111-111111111111")
	if err := packages.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET state='ready',completed_at=clock_timestamp(),ready_at=clock_timestamp(),media_expires_at=clock_timestamp()+interval '1 day',final_prefix='recordings/packages/package-grant-pool/final/attempt/' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if mode != "no-policy" {
		// Synthetic ready capture behind the package; production creation goes through
		// the capture-to-package workflow, which is outside this grant regression.
		if _, err := db.ExecContext(ctx, `INSERT INTO recording_captures(id,actor_id,recording_id,create_key,state,created_at,expires_at,receipts,broadcast_epoch) VALUES($1,'actor',$2,'grant-test','uploading',now(),now()+interval '1 hour','{}',1)`, p.ID, p.RecordingID); err != nil {
			t.Fatal(err)
		}
		policy := assets.RecordingBroadcastRange{RecordingID: p.RecordingID, Epoch: 1, RangeRevision: 2, StartSequence: boundary(0), EndSequenceExclusive: boundary(1), MemberState: "vod"}
		if mode == "revoked" || mode == "broadcast-disabled" {
			policy.Revoked = true
			policy.MemberState = "blocked"
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO recording_broadcast_ranges(capture_id,recording_id,epoch,range_revision,start_sequence,end_sequence_exclusive,revoked,member_state) VALUES($1,$2,1,2,0,1,$3,$4)`, p.ID, p.RecordingID, policy.Revoked, policy.MemberState); err != nil {
			t.Fatal(err)
		}
		history, _ := json.Marshal([]assets.RecordingLiveSegment{{Sequence: 0, Renditions: map[string]assets.RecordingLiveFragment{"1080p": {Start: 0, End: 10}}}})
		last := 0
		if mode == "projection-pending" {
			last = -1
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO recording_live(capture_id,segments,revision,published_revision,published_sequence) VALUES($1,$2,1,1,$3)`, p.ID, history, last); err != nil {
			t.Fatal(err)
		}
		if mode == "authority-mismatch" {
			policy.RangeRevision++
		}
		objects.values[p.ID] = broadcastRangeJSON(policy)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := assets.NewRecordingSigner(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), "test-key", "grant-pool-test")
	if err != nil {
		t.Fatal(err)
	}
	handler := httpapi.New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", httpapi.WorkloadAuthConfig{}, nil).
		WithRecordingPackages(assets.NewRecordingPackageService(packages, nil, time.Now)).
		WithRecordingSigner(signer)
	if mode != "broadcast-disabled" {
		handler.WithRecordingBroadcasts(NewRecordingCaptureStore(db).WithBroadcastObjects(objects))
	}
	h := handler.Routes()
	db.SetMaxOpenConns(1)
	grantCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"recordingId": p.RecordingID, "scopeId": "22222222-2222-4222-8222-222222222222", "recordingExpiresAt": time.Now().Add(time.Hour)})
	request := httptest.NewRequest("POST", "/priv/recording-packages/"+p.ID+"/grant", strings.NewReader(string(body))).WithContext(grantCtx)
	request.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	want := 200
	if mode == "revoked" || mode == "authority-mismatch" || mode == "projection-pending" {
		want = 409
	}
	if response.Code != want {
		t.Fatalf("grant HTTP %d, pool waits=%d: %s", response.Code, db.Stats().WaitCount, response.Body.String())
	}
	if db.Stats().WaitCount != 0 {
		t.Fatalf("nested pool checkout: %+v", db.Stats())
	}
}
