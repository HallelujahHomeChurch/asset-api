package assets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"
)

func TestCompleteUploadValidatesObservedBlobAndLeavesDownloadPending(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)

	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_version",
		OwnerID: "version-1", Purpose: "pdf", Locale: "zh-Hant", OriginalFileName: "weekly.pdf",
		ExpectedMIMEType: "application/pdf", MaxSizeBytes: 5 << 20, Visibility: VisibilityPublic,
	}, "request-1")
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("%PDF-1.7\nweekly bulletin")
	blobs.objects[created.Session.StagingObjectKey] = payload
	sum := sha256.Sum256(payload)
	asset, err := service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
		SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "application/pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if asset.ScanStatus != ScanPending {
		t.Fatalf("scan status = %s", asset.ScanStatus)
	}
	if created.Session.StagingObjectKey == "" || created.Session.StagingObjectKey == asset.ObjectKey {
		t.Fatalf("staging=%q final=%q", created.Session.StagingObjectKey, asset.ObjectKey)
	}
	if _, ok := blobs.objects[created.Session.StagingObjectKey]; ok {
		t.Fatal("staging object remains after completion")
	}
	if _, ok := blobs.objects[asset.ObjectKey]; !ok {
		t.Fatal("final object is missing after completion")
	}
	if _, err := service.OpenPublic(ctx, asset.ID, ByteRange{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("public download before clean scan: %v", err)
	}
}

func TestCreateUploadSessionReplaysIdempotencyKey(t *testing.T) {
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	input := CreateUploadInput{Namespace: "cms.news.cover", OwnerService: "hhc-web-api", OwnerType: "news", OwnerID: "news-1", OriginalFileName: "cover.jpg", ExpectedMIMEType: "image/jpeg", MaxSizeBytes: 5 << 20, Visibility: VisibilityPublic}

	first, err := service.CreateUploadSession(context.Background(), input, "news-cover-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateUploadSession(context.Background(), input, "news-cover-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Asset.ID != second.Asset.ID || second.Target.URL == "" {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}

func TestCreateUploadSessionRejectsIdempotencyReplayWithDifferentPurpose(t *testing.T) {
	repo := newMemoryRepository()
	service := NewService(repo, newMemoryBlobStore(), "https://www.alive.org.tw/api/assets", time.Now)
	input := CreateUploadInput{Namespace: "cms.news.cover", OwnerService: "hhc-web-api", OwnerType: "news", OwnerID: "news-1", Purpose: "cover", OriginalFileName: "cover.jpg", ExpectedMIMEType: "image/jpeg", MaxSizeBytes: 5 << 20, Visibility: VisibilityPublic}

	if _, err := service.CreateUploadSession(context.Background(), input, "semantic-replay"); err != nil {
		t.Fatal(err)
	}
	input.Purpose = "inline"
	if _, err := service.CreateUploadSession(context.Background(), input, "semantic-replay"); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay error = %v", err)
	}
}

func TestCreateUploadSessionEnforcesNamespaceOwnerSizeAndVisibility(t *testing.T) {
	service := NewService(newMemoryRepository(), newMemoryBlobStore(), "https://www.alive.org.tw/api/assets", time.Now)
	base := CreateUploadInput{Namespace: "account.avatar", OwnerService: "account-api", OwnerType: "user", OwnerID: "user-1", Purpose: "avatar", OriginalFileName: "avatar.jpg", ExpectedMIMEType: "image/jpeg", MaxSizeBytes: 1 << 20, Visibility: VisibilityPublic}

	tests := []CreateUploadInput{
		func() CreateUploadInput { value := base; value.OwnerService = "hhc-web-api"; return value }(),
		func() CreateUploadInput { value := base; value.MaxSizeBytes = (1 << 20) + 1; return value }(),
		func() CreateUploadInput { value := base; value.Visibility = VisibilityPrivate; return value }(),
	}
	for index, input := range tests {
		if _, err := service.CreateUploadSession(context.Background(), input, "invalid-policy-"+string(rune('a'+index))); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("case %d error = %v", index, err)
		}
	}
}

func TestCompleteUploadRejectsMIMEAndSizeSpoofing(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_version",
		OwnerID: "version-2", Purpose: "pdf", OriginalFileName: "weekly.pdf",
		ExpectedMIMEType: "application/pdf", MaxSizeBytes: 100, Visibility: VisibilityPrivate,
	}, "request-2")
	if err != nil {
		t.Fatal(err)
	}
	blobs.objects[created.Session.StagingObjectKey] = []byte("<html>not a pdf</html>")

	_, err = service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
		SizeBytes: 1, ChecksumSHA256: "invalid", MIMEType: "application/pdf",
	})
	if !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("expected invalid upload, got %v", err)
	}
}

func TestCleanScanAndPublicGrantEnableStableDownload(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	asset := completedAsset(t, ctx, service, blobs, VisibilityPublic)
	if err := service.ApplyScanResult(ctx, ScanResult{EventID: "scan-1", AssetID: asset.ID, Status: ScanClean}); err != nil {
		t.Fatal(err)
	}
	grant, err := service.CreateGrant(ctx, asset.ID, CreateGrantInput{
		SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "grant-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if grant.ID == "" {
		t.Fatal("missing grant id")
	}
	download, err := service.OpenPublic(ctx, asset.ID, ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	defer download.Body.Close()
	body, _ := io.ReadAll(download.Body)
	if !bytes.HasPrefix(body, []byte("%PDF")) {
		t.Fatalf("unexpected body %q", body)
	}
	if got := service.PublicURL(asset.ID); got != "https://www.alive.org.tw/api/assets/public/"+asset.ID {
		t.Fatalf("public url = %s", got)
	}
}

func TestPublicGrantAlsoProtectsStableDerivative(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	asset := Asset{ID: "image-1", ObjectKey: "assets/image-1/original", UploadStatus: UploadCompleted, ScanStatus: ScanClean, ProcessingStatus: ProcessingReady, Visibility: VisibilityPublic, DetectedMIMEType: "image/png", SizeBytes: 8}
	repo.assets[asset.ID] = asset
	repo.derivatives[asset.ID+":large"] = Derivative{AssetID: asset.ID, Variant: "large", ObjectKey: "assets/image-1/derivatives/large.jpg", MIMEType: "image/jpeg", SizeBytes: 10}
	blobs.objects["assets/image-1/derivatives/large.jpg"] = []byte("jpeg-bytes")

	if _, err := service.OpenPublicVariant(ctx, asset.ID, "large", ByteRange{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("derivative was public without grant: %v", err)
	}
	if _, err := service.CreateGrant(ctx, asset.ID, CreateGrantInput{SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "image-grant"}); err != nil {
		t.Fatal(err)
	}
	download, err := service.OpenPublicVariant(ctx, asset.ID, "large", ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	defer download.Body.Close()
	if download.ContentType != "image/jpeg" {
		t.Fatalf("content type=%s", download.ContentType)
	}
}

func TestInfectedScanDeniesDownloadAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	asset := completedAsset(t, ctx, service, blobs, VisibilityPublic)
	_, _ = service.CreateGrant(ctx, asset.ID, CreateGrantInput{SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "grant-2"})
	result := ScanResult{EventID: "scan-infected", AssetID: asset.ID, Status: ScanInfected, Details: "Malware detected"}
	if err := service.ApplyScanResult(ctx, result); err != nil {
		t.Fatal(err)
	}
	if err := service.ApplyScanResult(ctx, result); err != nil {
		t.Fatalf("duplicate event should be idempotent: %v", err)
	}
	if _, err := service.OpenPublic(ctx, asset.ID, ByteRange{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("infected asset was downloadable: %v", err)
	}
}

func TestScanResultRejectsChangedCommittedETag(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	asset := completedAsset(t, ctx, service, blobs, VisibilityPublic)
	originalETag := asset.ETag
	asset.ETag = "mutated"
	repo.assets[asset.ID] = asset

	err := service.ApplyScanResult(ctx, ScanResult{EventID: "stale-scan", AssetID: asset.ID, Status: ScanClean, ETag: originalETag})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("scan error = %v", err)
	}
}

func TestPublicGrantRequiresCleanAsset(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	asset := completedAsset(t, ctx, service, blobs, VisibilityPublic)
	_, err := service.CreateGrant(ctx, asset.ID, CreateGrantInput{SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "pending-public"})
	if !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("pending grant error = %v", err)
	}
	if err := service.ApplyScanResult(ctx, ScanResult{EventID: "clean-grant", AssetID: asset.ID, Status: ScanClean}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateGrant(ctx, asset.ID, CreateGrantInput{SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "clean-public"}); err != nil {
		t.Fatal(err)
	}
}

func completedAsset(t *testing.T, ctx context.Context, service *Service, blobs *memoryBlobStore, visibility Visibility) Asset {
	t.Helper()
	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_version",
		OwnerID: "version-complete", Purpose: "pdf", OriginalFileName: "weekly.pdf",
		ExpectedMIMEType: "application/pdf", MaxSizeBytes: 5 << 20, Visibility: visibility,
	}, "request-complete")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("%PDF-1.7\nclean weekly bulletin")
	blobs.objects[created.Session.StagingObjectKey] = payload
	sum := sha256.Sum256(payload)
	asset, err := service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "application/pdf"})
	if err != nil {
		t.Fatal(err)
	}
	return asset
}

type memoryRepository struct {
	assets      map[string]Asset
	sessions    map[string]UploadSession
	grants      map[string]Grant
	events      map[string]struct{}
	derivatives map[string]Derivative
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{assets: map[string]Asset{}, sessions: map[string]UploadSession{}, grants: map[string]Grant{}, events: map[string]struct{}{}, derivatives: map[string]Derivative{}}
}

func (r *memoryRepository) CreateUpload(_ context.Context, asset Asset, session UploadSession) error {
	r.assets[asset.ID] = asset
	r.sessions[asset.ID] = session
	return nil
}
func (r *memoryRepository) GetAsset(_ context.Context, id string) (Asset, error) {
	value, ok := r.assets[id]
	if !ok {
		return Asset{}, ErrNotFound
	}
	return value, nil
}
func (r *memoryRepository) GetUploadSession(_ context.Context, assetID string) (UploadSession, error) {
	value, ok := r.sessions[assetID]
	if !ok {
		return UploadSession{}, ErrNotFound
	}
	return value, nil
}
func (r *memoryRepository) FindUploadByIdempotency(_ context.Context, caller, operation, key string) (Asset, UploadSession, error) {
	for assetID, session := range r.sessions {
		if session.CallerService == caller && session.Operation == operation && session.IdempotencyKey == key {
			return r.assets[assetID], session, nil
		}
	}
	return Asset{}, UploadSession{}, ErrNotFound
}
func (r *memoryRepository) CompleteUpload(_ context.Context, asset Asset, session UploadSession) error {
	r.assets[asset.ID] = asset
	r.sessions[asset.ID] = session
	return nil
}
func (r *memoryRepository) CreateGrant(_ context.Context, grant Grant) (Grant, error) {
	for _, value := range r.grants {
		if value.CallerService == grant.CallerService && value.Operation == grant.Operation && value.IdempotencyKey == grant.IdempotencyKey {
			return value, nil
		}
	}
	r.grants[grant.ID] = grant
	return grant, nil
}
func (r *memoryRepository) RevokeGrant(_ context.Context, assetID, grantID string, _ time.Time) error {
	value, ok := r.grants[grantID]
	if !ok || value.AssetID != assetID {
		return ErrNotFound
	}
	value.RevokedAt = time.Now()
	r.grants[grantID] = value
	return nil
}
func (r *memoryRepository) HasActiveGrant(_ context.Context, assetID string, subject SubjectType, subjectID string, permission Permission, now time.Time) (bool, error) {
	for _, grant := range r.grants {
		if grant.AssetID == assetID && grant.SubjectType == subject && grant.SubjectID == subjectID && grant.Permission == permission && grant.RevokedAt.IsZero() && (grant.ExpiresAt.IsZero() || grant.ExpiresAt.After(now)) {
			return true, nil
		}
	}
	return false, nil
}
func (r *memoryRepository) ApplyScanResult(_ context.Context, result ScanResult, now time.Time) (bool, error) {
	if _, ok := r.events[result.EventID]; ok {
		return false, nil
	}
	asset, ok := r.assets[result.AssetID]
	if !ok {
		return false, ErrNotFound
	}
	if asset.ScanStatus != ScanPending || asset.ETag != result.ETag {
		return false, ErrConflict
	}
	r.events[result.EventID] = struct{}{}
	asset.ScanStatus = result.Status
	asset.ScanDetails = result.Details
	asset.UpdatedAt = now
	r.assets[asset.ID] = asset
	return true, nil
}
func (r *memoryRepository) ClaimPendingScan(_ context.Context, _ time.Time, _ time.Duration) (Asset, bool, error) {
	for id, asset := range r.assets {
		if asset.UploadStatus == UploadCompleted && asset.ScanStatus == ScanPending {
			asset.ScanAttempts++
			r.assets[id] = asset
			return asset, true, nil
		}
	}
	return Asset{}, false, nil
}
func (r *memoryRepository) ScheduleScanRetry(_ context.Context, assetID, details string, _, now time.Time) error {
	asset, ok := r.assets[assetID]
	if !ok {
		return ErrNotFound
	}
	asset.ScanDetails = details
	asset.UpdatedAt = now
	r.assets[assetID] = asset
	return nil
}
func (r *memoryRepository) GetDerivative(_ context.Context, assetID, variant string) (Derivative, error) {
	value, ok := r.derivatives[assetID+":"+variant]
	if !ok {
		return Derivative{}, ErrNotFound
	}
	return value, nil
}

type memoryBlobStore struct{ objects map[string][]byte }

func newMemoryBlobStore() *memoryBlobStore { return &memoryBlobStore{objects: map[string][]byte{}} }
func (b *memoryBlobStore) CreateUploadTarget(_ context.Context, objectKey string, _ int64, expiresAt time.Time) (UploadTarget, error) {
	return UploadTarget{URL: "https://upload.example/" + objectKey, Method: "PUT", ExpiresAt: expiresAt, Headers: map[string]string{"x-ms-blob-type": "BlockBlob"}}, nil
}
func (b *memoryBlobStore) Inspect(_ context.Context, objectKey string) (BlobProperties, error) {
	value, ok := b.objects[objectKey]
	if !ok {
		return BlobProperties{}, ErrNotFound
	}
	properties := inspectBytes(value)
	properties.ETag = "etag-" + properties.ChecksumSHA256
	return properties, nil
}
func (b *memoryBlobStore) Commit(ctx context.Context, stagingObjectKey, finalObjectKey string) (BlobProperties, error) {
	value, ok := b.objects[stagingObjectKey]
	if !ok {
		if _, finalExists := b.objects[finalObjectKey]; finalExists {
			return b.Inspect(ctx, finalObjectKey)
		}
		return BlobProperties{}, ErrNotFound
	}
	if _, exists := b.objects[finalObjectKey]; exists {
		return BlobProperties{}, ErrConflict
	}
	b.objects[finalObjectKey] = append([]byte(nil), value...)
	delete(b.objects, stagingObjectKey)
	return b.Inspect(ctx, finalObjectKey)
}
func (b *memoryBlobStore) Open(ctx context.Context, objectKey string, _ ByteRange, expectedETag string) (BlobDownload, error) {
	value, ok := b.objects[objectKey]
	if !ok {
		return BlobDownload{}, ErrNotFound
	}
	properties, _ := b.Inspect(ctx, objectKey)
	if expectedETag != "" && properties.ETag != expectedETag {
		return BlobDownload{}, ErrInvalidUpload
	}
	return BlobDownload{Body: io.NopCloser(bytes.NewReader(value)), Size: int64(len(value)), ContentType: "application/pdf", ETag: properties.ETag}, nil
}
func (b *memoryBlobStore) Delete(_ context.Context, objectKey string) error {
	delete(b.objects, objectKey)
	return nil
}
func (b *memoryBlobStore) Put(_ context.Context, objectKey string, reader io.Reader, size int64, _ string) (BlobProperties, error) {
	value, err := io.ReadAll(reader)
	if err != nil || int64(len(value)) != size {
		return BlobProperties{}, ErrInvalidUpload
	}
	b.objects[objectKey] = value
	properties := inspectBytes(value)
	properties.ETag = "etag-derivative"
	return properties, nil
}
