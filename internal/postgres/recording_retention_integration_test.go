package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/auditclient"
	"hhc/asset-api/internal/migrations"
)

func TestRecordingRetentionActivationAndGrantGrace(t *testing.T) {
	ctx := auditclient.WithProvenance(context.Background(), "user", "11111111-1111-4111-8111-111111111111", "retention-test")
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "retention-package", "actor-a", "retention-recording")
	if err := store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	uploaded := time.Now().UTC().Add(-20 * 24 * time.Hour).Truncate(time.Microsecond)
	if err := store.Freeze(ctx, p.ID, uploaded); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishPackageValidation(ctx, p.ID, claim.ClaimID, true, ""); err != nil {
		t.Fatal(err)
	}
	policy, err := store.GetRetentionPolicy(ctx)
	if err != nil || policy.RetentionDays != 30 || policy.ActivatedAt != nil {
		t.Fatalf("initial policy: %+v %v", policy, err)
	}
	preview, err := store.PreviewRetentionPolicy(ctx, 14)
	if err != nil || preview.AffectedCount != 1 || preview.AffectedBytes != p.SizeBytes {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	input := assets.UpdateRecordingRetentionInput{PreviewID: preview.PreviewID, ExpectedRevision: policy.Revision, RetentionDays: 14, IdempotencyKey: "shorten"}
	policy, err = store.UpdateRetentionPolicy(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.UpdateRetentionPolicy(ctx, input)
	if err != nil || replay.Revision != policy.Revision {
		t.Fatalf("idempotent replay: %+v %v", replay, err)
	}
	got, err := store.Get(ctx, p.ID)
	if err != nil || got.UploadedAt == nil || !got.UploadedAt.Equal(uploaded) || !got.MediaExpiresAt.Equal(uploaded.Add(14*24*time.Hour)) {
		t.Fatalf("retroactive expiry: %+v %v", got, err)
	}
	if err := store.CleanupPackage(ctx, p.ID, func(_ context.Context, keys []string) error {
		for _, key := range keys {
			if strings.Contains(key, "/final/") {
				t.Fatalf("deleted beneath existing grant: %s", key)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Once cleanup fences the package, a longer policy must never revive it.
	preview, err = store.PreviewRetentionPolicy(ctx, 60)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.UpdateRetentionPolicy(ctx, assets.UpdateRecordingRetentionInput{PreviewID: preview.PreviewID, ExpectedRevision: policy.Revision, RetentionDays: 60, IdempotencyKey: "extend"})
	if err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(ctx, p.ID)
	if err != nil || got.State != "expired" {
		t.Fatalf("terminal package revived: %+v %v", got, err)
	}
	if _, err := store.PreviewRetentionPolicy(ctx, 0); !errors.Is(err, assets.ErrInvalidInput) {
		t.Fatalf("invalid days: %v", err)
	}
}

func TestRecordingRetentionNewBrowserAndCLIPackagesUseUploadCompletion(t *testing.T) {
	ctx := auditclient.WithProvenance(context.Background(), "user", "11111111-1111-4111-8111-111111111111", "retention-producers")
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	packages := NewRecordingPackageStore(db)
	preview, err := packages.PreviewRetentionPolicy(ctx, 60)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := packages.UpdateRetentionPolicy(ctx, assets.UpdateRecordingRetentionInput{RetentionDays: 60, ExpectedRevision: preview.Revision, PreviewID: preview.PreviewID, IdempotencyKey: "activate"})
	if err != nil {
		t.Fatal(err)
	}
	uploaded := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	cli := testRecordingPackage(t, "retention-cli", "cli-actor", "cli-recording")
	if err := packages.Create(ctx, cli); err != nil {
		t.Fatal(err)
	}
	if err := packages.Freeze(ctx, cli.ID, uploaded); err != nil {
		t.Fatal(err)
	}
	claim, err := packages.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := packages.FinishPackageValidation(ctx, cli.ID, claim.ClaimID, true, ""); err != nil {
		t.Fatal(err)
	}
	sources := NewRecordingSourceStore(db)
	svc := assets.NewRecordingSourceService(sources, &sourceBlobFixture{}, time.Now)
	source, err := svc.Create(ctx, assets.CreateRecordingSourceInput{ActorID: "browser-actor", RecordingID: "browser-recording", FileName: "source.mp4", SizeBytes: 5, ChecksumSHA256: strings.Repeat("a", 64), IdempotencyKey: "browser"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(ctx, source.ID, source.ActorID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_sources SET completed_at=$2,retry_until=$2::timestamptz+interval '7 days' WHERE id=$1`, source.ID, uploaded); err != nil {
		t.Fatal(err)
	}
	sourceClaim, err := sources.ClaimSourceProcessing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	copy := assets.RecordingSourceCopy{Key: "recording-sources/" + source.ID + "/final/" + sourceClaim.ClaimID + "/source", State: "success", ETag: "verified", SizeBytes: 5}
	if err := sources.CheckpointSourceCopy(ctx, source.ID, sourceClaim.ClaimID, copy); err != nil {
		t.Fatal(err)
	}
	// Both kinds of accepted but unfinished upload must appear in impact preview.
	oldUpload := time.Now().UTC().Add(-2 * 24 * time.Hour).Truncate(time.Microsecond)
	if _, err := db.ExecContext(ctx, `UPDATE recording_sources SET completed_at=$2,retry_until=$2::timestamptz+interval '7 days' WHERE id=$1`, source.ID, oldUpload); err != nil {
		t.Fatal(err)
	}
	pending := testRecordingPackage(t, "retention-pending", "pending-actor", "pending-recording")
	if err := packages.Create(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if err := packages.Freeze(ctx, pending.ID, oldUpload); err != nil {
		t.Fatal(err)
	}
	impact, err := packages.PreviewRetentionPolicy(ctx, 1)
	if err != nil || impact.AffectedCount != 2 || impact.AffectedBytes != pending.SizeBytes+source.SizeBytes {
		t.Fatalf("unfinished uploads omitted: %+v %v", impact, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_sources SET completed_at=$2,retry_until=$2::timestamptz+interval '7 days' WHERE id=$1`, source.ID, uploaded); err != nil {
		t.Fatal(err)
	}
	if err := sources.FinishSourceProcessing(ctx, source.ID, sourceClaim.ClaimID, cli.Inventory); err != nil {
		t.Fatal(err)
	}
	bindings := []assets.RecordingLifecycleBinding{{PackageID: cli.ID, RecordingID: cli.RecordingID}, {PackageID: sourceClaim.ClaimID, RecordingID: source.RecordingID}}
	snapshot, err := packages.RecordingLifecycle(ctx, bindings)
	if err != nil || len(snapshot.Items) != 2 || snapshot.Policy.Revision != policy.Revision {
		t.Fatalf("snapshot: %+v %v", snapshot, err)
	}
	for _, p := range snapshot.Items {
		if p.State != "ready" || p.UploadedAt == nil || !p.UploadedAt.Equal(uploaded) || p.MediaExpiresAt == nil || !p.MediaExpiresAt.Equal(uploaded.Add(60*24*time.Hour)) || p.RetentionRevision != policy.Revision {
			t.Fatalf("completion reset: %+v", p)
		}
	}
	bindings[0].RecordingID = "wrong-recording"
	if _, err := packages.RecordingLifecycle(ctx, bindings); !errors.Is(err, assets.ErrForbidden) {
		t.Fatalf("wrong binding accepted: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET completed_at=$2,media_expires_at=$2::timestamptz+interval '60 days' WHERE id=$1`, sourceClaim.ClaimID, oldUpload); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_sources SET completed_at=$2,retry_until=$2::timestamptz+interval '7 days' WHERE id=$1`, source.ID, oldUpload); err != nil {
		t.Fatal(err)
	}
	impact, err = packages.PreviewRetentionPolicy(ctx, 1)
	if err != nil || impact.AffectedCount != 2 {
		t.Fatalf("materialized source double counted: %+v %v", impact, err)
	}
}

func TestRecordingRetentionExtendsWithoutResetAndFailsClosed(t *testing.T) {
	ctx := auditclient.WithProvenance(context.Background(), "user", "11111111-1111-4111-8111-111111111111", "retention-test")
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "extend-package", "actor-a", "extend-recording")
	if err := store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	uploaded := time.Now().UTC().Add(-20 * 24 * time.Hour).Truncate(time.Microsecond)
	if err := store.Freeze(ctx, p.ID, uploaded); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishPackageValidation(ctx, p.ID, claim.ClaimID, true, ""); err != nil {
		t.Fatal(err)
	}
	preview, err := store.PreviewRetentionPolicy(ctx, 60)
	if err != nil {
		t.Fatal(err)
	}
	in := assets.UpdateRecordingRetentionInput{RetentionDays: 60, ExpectedRevision: preview.Revision, PreviewID: preview.PreviewID, IdempotencyKey: "extend"}
	other := auditclient.WithProvenance(context.Background(), "user", "22222222-2222-4222-8222-222222222222", "other-admin")
	if _, err := store.UpdateRetentionPolicy(other, in); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("other actor accepted preview: %v", err)
	}
	if err := store.WithSessionLock(ctx, p.ID, func(ctx context.Context) error {
		_, err := store.UpdateRetentionPolicy(ctx, in)
		if !errors.Is(err, assets.ErrConflict) {
			t.Fatalf("policy must not cross grant/cleanup lock: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE audit_outboxes RENAME TO audit_outboxes_unavailable`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRetentionPolicy(ctx, in); !errors.Is(err, assets.ErrAuditUnavailable) {
		t.Fatalf("audit fail-open: %v", err)
	}
	policy, err := store.GetRetentionPolicy(ctx)
	if err != nil || policy.Revision != 1 || policy.ActivatedAt != nil {
		t.Fatalf("partial mutation: %+v %v", policy, err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE audit_outboxes_unavailable RENAME TO audit_outboxes`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRetentionPolicy(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, p.ID)
	if err != nil || got.State != "ready" || !got.MediaExpiresAt.Equal(uploaded.Add(60*24*time.Hour)) || !got.UploadedAt.Equal(uploaded) {
		t.Fatalf("extension: %+v %v", got, err)
	}
	if _, err := store.UpdateRetentionPolicy(ctx, assets.UpdateRecordingRetentionInput{RetentionDays: 14, ExpectedRevision: 1, PreviewID: preview.PreviewID, IdempotencyKey: "stale"}); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	// Simulate damaged legacy data; normal writes are already protected by CHECK.
	if _, err := db.ExecContext(ctx, `ALTER TABLE recording_packages DROP CONSTRAINT recording_packages_check2`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET completed_at=NULL WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PreviewRetentionPolicy(ctx, 30); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("missing completion guessed: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM recording_retention_policy`); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupPackage(ctx, p.ID, func(context.Context, []string) error { t.Fatal("deleted without policy"); return nil }); err == nil {
		t.Fatal("missing policy must fail closed")
	}
}
