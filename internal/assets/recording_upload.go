package assets

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"hhc/asset-api/internal/storage/r2"
)

const (
	RecordingMaxSizeBytes  int64 = 10_000_000_000
	RecordingPartSizeBytes int64 = 16 << 20
	RecordingMaxParts            = 597
	RecordingUploadTTL           = 24 * time.Hour
	RecordingPartURLTTL          = 15 * time.Minute
)

// RecordingUploadSession belongs exclusively to the private R2 recording path.
// It must never be passed to the Blob CreateUploadSession/scan pipeline.
type RecordingUploadSession struct {
	ID              string     `json:"id"`
	AssetVersionID  string     `json:"assetVersionId"`
	OwnerService    string     `json:"ownerService"`
	AdminID         string     `json:"adminId"`
	RecordingID     string     `json:"recordingId"`
	FileName        string     `json:"fileName"`
	ObjectKey       string     `json:"objectKey"`
	UploadID        string     `json:"uploadId"`
	IdempotencyKey  string     `json:"idempotencyKey"`
	Status          string     `json:"status"`
	SizeBytes       int64      `json:"sizeBytes"`
	ChecksumSHA256  string     `json:"checksumSHA256"`
	DurationSeconds float64    `json:"durationSeconds"`
	CreatedAt       time.Time  `json:"createdAt"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	UploadedAt      *time.Time `json:"uploadedAt"`
}

type CreateRecordingUploadInput struct {
	AdminID        string
	RecordingID    string
	FileName       string
	SizeBytes      int64
	ChecksumSHA256 string
	IdempotencyKey string
}

type RecordingUploadRepository interface {
	WithSessionLock(context.Context, string, func(context.Context) error) error
	FindByIdempotency(context.Context, string) (RecordingUploadSession, error)
	Create(context.Context, RecordingUploadSession) error
	Get(context.Context, string) (RecordingUploadSession, error)
	ClaimCompletion(context.Context, string) error
	MarkValidating(context.Context, string) error
	MarkCancelled(context.Context, string) error
	GetByVersion(context.Context, string) (RecordingUploadSession, error)
	MarkDeleted(context.Context, string) error
	ClaimDeletion(context.Context, string) error
	ClaimAbandonedDeletion(context.Context, string, time.Time) error
}

type RecordingObjectStore interface {
	Create(context.Context, string) (string, error)
	Abort(context.Context, string, string) error
	PresignPart(context.Context, string, string, int, time.Duration) (r2.PresignedPart, error)
	ListParts(context.Context, string, string) ([]r2.Part, error)
	Complete(context.Context, string, string, []r2.Part) error
	Head(context.Context, string) (int64, string, error)
	Delete(context.Context, string) error
}

type RecordingUploadService struct {
	repository RecordingUploadRepository
	objects    RecordingObjectStore
	now        func() time.Time
}

func NewRecordingUploadService(repository RecordingUploadRepository, objects RecordingObjectStore, now func() time.Time) *RecordingUploadService {
	return &RecordingUploadService{repository: repository, objects: objects, now: now}
}

func (s *RecordingUploadService) Get(ctx context.Context, sessionID, adminID string) (RecordingUploadSession, error) {
	session, err := s.repository.Get(ctx, sessionID)
	if err != nil {
		return RecordingUploadSession{}, err
	}
	if session.OwnerService != "hhc-web-api" || session.AdminID != adminID {
		return RecordingUploadSession{}, ErrForbidden
	}
	if session.Status == "completing" {
		completed, err := s.Complete(ctx, session.ID, adminID)
		if errors.Is(err, ErrConflict) {
			return s.repository.Get(ctx, session.ID)
		}
		return completed, err
	}
	return session, nil
}

func (s *RecordingUploadService) Abort(ctx context.Context, sessionID, adminID string) error {
	session, err := s.Get(ctx, sessionID, adminID)
	if err != nil {
		return err
	}
	if session.Status == "cancelled" {
		return nil
	}
	if session.Status != "created" {
		return ErrConflict
	}
	if err := s.repository.MarkCancelled(ctx, sessionID); err != nil {
		return err
	}
	return s.objects.Abort(ctx, session.ObjectKey, session.UploadID)
}

func (s *RecordingUploadService) DeleteAsset(ctx context.Context, versionID string) error {
	session, err := s.repository.GetByVersion(ctx, versionID)
	if err != nil {
		return err
	}
	return s.repository.WithSessionLock(ctx, session.ID, func(ctx context.Context) error { return s.deleteAsset(ctx, versionID) })
}

func (s *RecordingUploadService) deleteAsset(ctx context.Context, versionID string) error {
	session, err := s.repository.GetByVersion(ctx, versionID)
	if err != nil {
		return err
	}
	if session.OwnerService != "hhc-web-api" {
		return ErrForbidden
	}
	if session.Status == "deleted" {
		return nil
	}
	if err := s.repository.ClaimDeletion(ctx, session.ID); err != nil {
		return err
	}
	return s.deleteClaimed(ctx, session)
}

func (s *RecordingUploadService) deleteClaimed(ctx context.Context, session RecordingUploadSession) error {
	if err := s.objects.Abort(ctx, session.ObjectKey, session.UploadID); err != nil {
		return err
	}
	if err := s.objects.Delete(ctx, session.ObjectKey); err != nil {
		return err
	}
	return s.repository.MarkDeleted(ctx, session.ID)
}

func (s *RecordingUploadService) DeleteAbandoned(ctx context.Context, versionID string, now time.Time) error {
	session, err := s.repository.GetByVersion(ctx, versionID)
	if err != nil {
		return err
	}
	return s.repository.WithSessionLock(ctx, session.ID, func(ctx context.Context) error { return s.deleteAbandoned(ctx, versionID, now) })
}

func (s *RecordingUploadService) deleteAbandoned(ctx context.Context, versionID string, now time.Time) error {
	session, err := s.repository.GetByVersion(ctx, versionID)
	if err != nil {
		return err
	}
	if session.OwnerService != "hhc-web-api" {
		return ErrForbidden
	}
	if session.Status == "completing" {
		size, _, err := s.objects.Head(ctx, session.ObjectKey)
		if err == nil && size == session.SizeBytes {
			_, err = s.markValidating(ctx, session)
			return err
		}
		if err != nil && !errors.Is(err, r2.ErrNotFound) {
			return err
		}
	}
	// Recheck eligibility atomically; a previously selected candidate may now
	// be validating/ready. Never route this through unconditional owner deletion.
	if err := s.repository.ClaimAbandonedDeletion(ctx, session.ID, now); err != nil {
		return err
	}
	return s.deleteClaimed(ctx, session)
}

func (s *RecordingUploadService) Create(ctx context.Context, input CreateRecordingUploadInput) (RecordingUploadSession, error) {
	if !mediaID.MatchString(input.AdminID) || !mediaID.MatchString(input.RecordingID) || input.IdempotencyKey == "" || !strings.HasSuffix(strings.ToLower(strings.TrimSpace(input.FileName)), ".mp4") {
		return RecordingUploadSession{}, ErrInvalidInput
	}
	if _, err := RecordingPartCount(input.SizeBytes); err != nil {
		return RecordingUploadSession{}, err
	}
	if len(input.ChecksumSHA256) != 64 {
		return RecordingUploadSession{}, ErrInvalidInput
	}
	if _, err := hex.DecodeString(input.ChecksumSHA256); err != nil {
		return RecordingUploadSession{}, ErrInvalidInput
	}
	if existing, err := s.repository.FindByIdempotency(ctx, input.IdempotencyKey); err == nil {
		if existing.CanResume("hhc-web-api", input.AdminID, input.RecordingID, input.SizeBytes, input.ChecksumSHA256, s.now()) && existing.Status == "created" {
			return existing, nil
		}
		return RecordingUploadSession{}, ErrConflict
	} else if !errors.Is(err, ErrNotFound) {
		return RecordingUploadSession{}, err
	}
	now := s.now().UTC()
	assetVersionID := newID()
	key := "recordings/" + assetVersionID + ".mp4"
	uploadID, err := s.objects.Create(ctx, key)
	if err != nil {
		return RecordingUploadSession{}, fmt.Errorf("create R2 upload: %w", err)
	}
	session := RecordingUploadSession{
		ID: newID(), AssetVersionID: assetVersionID, OwnerService: "hhc-web-api", AdminID: input.AdminID,
		RecordingID: input.RecordingID, FileName: sanitizeFileName(input.FileName), ObjectKey: key,
		UploadID: uploadID, IdempotencyKey: input.IdempotencyKey, Status: "created",
		SizeBytes: input.SizeBytes, ChecksumSHA256: strings.ToLower(input.ChecksumSHA256),
		CreatedAt: now, ExpiresAt: now.Add(RecordingUploadTTL),
	}
	if err := s.repository.Create(ctx, session); err != nil {
		_ = s.objects.Abort(ctx, key, uploadID)
		return RecordingUploadSession{}, err
	}
	return session, nil
}

func (s *RecordingUploadService) SignPart(ctx context.Context, sessionID, adminID string, number int) (r2.PresignedPart, error) {
	session, err := s.repository.Get(ctx, sessionID)
	if err != nil {
		return r2.PresignedPart{}, err
	}
	if session.OwnerService != "hhc-web-api" || session.AdminID != adminID {
		return r2.PresignedPart{}, ErrForbidden
	}
	now := s.now()
	if session.Status != "created" || !now.Before(session.ExpiresAt) {
		return r2.PresignedPart{}, ErrConflict
	}
	count, err := RecordingPartCount(session.SizeBytes)
	if err != nil || number < 1 || number > count {
		return r2.PresignedPart{}, ErrInvalidInput
	}
	ttl := RecordingPartURLTTL
	if remaining := session.ExpiresAt.Sub(now); remaining < ttl {
		ttl = remaining
	}
	return s.objects.PresignPart(ctx, session.ObjectKey, session.UploadID, number, ttl)
}

func (s *RecordingUploadService) UploadedParts(ctx context.Context, sessionID, adminID string) ([]int, error) {
	session, err := s.Get(ctx, sessionID, adminID)
	if err != nil {
		return nil, err
	}
	if session.Status != "created" || !s.now().Before(session.ExpiresAt) {
		return nil, ErrConflict
	}
	parts, err := s.objects.ListParts(ctx, session.ObjectKey, session.UploadID)
	if err != nil {
		return nil, err
	}
	count, err := RecordingPartCount(session.SizeBytes)
	if err != nil {
		return nil, err
	}
	numbers := make([]int, 0, len(parts))
	for _, part := range parts {
		if part.Number < 1 || part.Number > count || part.ETag == "" {
			continue
		}
		want := RecordingPartSizeBytes
		if part.Number == count {
			want = session.SizeBytes - int64(count-1)*RecordingPartSizeBytes
		}
		if part.Size == want {
			numbers = append(numbers, part.Number)
		}
	}
	return numbers, nil
}

func (s *RecordingUploadService) Complete(ctx context.Context, sessionID, adminID string) (RecordingUploadSession, error) {
	var value RecordingUploadSession
	err := s.repository.WithSessionLock(ctx, sessionID, func(ctx context.Context) error {
		var err error
		value, err = s.complete(ctx, sessionID, adminID)
		return err
	})
	return value, err
}

func (s *RecordingUploadService) complete(ctx context.Context, sessionID, adminID string) (RecordingUploadSession, error) {
	session, err := s.repository.Get(ctx, sessionID)
	if err != nil {
		return RecordingUploadSession{}, err
	}
	if session.OwnerService != "hhc-web-api" || session.AdminID != adminID {
		return RecordingUploadSession{}, ErrForbidden
	}
	if session.Status == "validating" || session.Status == "ready" {
		return session, nil
	}
	// Recover an already committed object even after the multipart deadline.
	if session.Status == "completing" {
		if size, _, err := s.objects.Head(ctx, session.ObjectKey); err == nil && size == session.SizeBytes {
			return s.markValidating(ctx, session)
		}
	}
	if (session.Status != "created" && session.Status != "completing") || !s.now().Before(session.ExpiresAt) {
		return RecordingUploadSession{}, ErrConflict
	}
	parts, err := s.objects.ListParts(ctx, session.ObjectKey, session.UploadID)
	if err != nil {
		// A prior Complete may have committed R2 before the DB transition.
		if size, _, headErr := s.objects.Head(ctx, session.ObjectKey); headErr == nil && size == session.SizeBytes && session.Status == "completing" {
			return s.markValidating(ctx, session)
		}
		return RecordingUploadSession{}, err
	}
	count, err := RecordingPartCount(session.SizeBytes)
	if err != nil || len(parts) != count {
		return RecordingUploadSession{}, ErrInvalidUpload
	}
	for index, part := range parts {
		want := RecordingPartSizeBytes
		if index == count-1 {
			want = session.SizeBytes - int64(index)*RecordingPartSizeBytes
		}
		if part.Number != index+1 || part.Size != want || part.ETag == "" {
			return RecordingUploadSession{}, ErrInvalidUpload
		}
	}
	if session.Status == "created" {
		if err := s.repository.ClaimCompletion(ctx, session.ID); err != nil {
			return RecordingUploadSession{}, err
		}
		session.Status = "completing"
	}
	if err := s.objects.Complete(ctx, session.ObjectKey, session.UploadID, parts); err != nil {
		if size, _, headErr := s.objects.Head(ctx, session.ObjectKey); headErr != nil || size != session.SizeBytes {
			return RecordingUploadSession{}, err
		}
	}
	size, _, err := s.objects.Head(ctx, session.ObjectKey)
	if err != nil {
		return RecordingUploadSession{}, err
	}
	if size != session.SizeBytes {
		return RecordingUploadSession{}, ErrInvalidUpload
	}
	return s.markValidating(ctx, session)
}

func (s *RecordingUploadService) markValidating(ctx context.Context, session RecordingUploadSession) (RecordingUploadSession, error) {
	if err := s.repository.MarkValidating(ctx, session.ID); err != nil {
		latest, getErr := s.repository.Get(ctx, session.ID)
		if getErr == nil && (latest.Status == "validating" || latest.Status == "ready") {
			return latest, nil
		}
		return RecordingUploadSession{}, err
	}
	return s.repository.Get(ctx, session.ID)
}

func RecordingPartCount(size int64) (int, error) {
	if size <= 0 || size > RecordingMaxSizeBytes {
		return 0, ErrInvalidInput
	}
	parts := int((size + RecordingPartSizeBytes - 1) / RecordingPartSizeBytes)
	if parts > RecordingMaxParts {
		return 0, ErrInvalidInput
	}
	return parts, nil
}

func (s RecordingUploadSession) CanResume(ownerService, adminID, recordingID string, size int64, checksum string, now time.Time) bool {
	if s.OwnerService != "hhc-web-api" || ownerService != s.OwnerService || adminID == "" || adminID != s.AdminID || recordingID == "" || recordingID != s.RecordingID || !now.Before(s.ExpiresAt) || size != s.SizeBytes {
		return false
	}
	if len(checksum) != 64 || len(s.ChecksumSHA256) != 64 {
		return false
	}
	if _, err := hex.DecodeString(checksum); err != nil {
		return false
	}
	return strings.EqualFold(checksum, s.ChecksumSHA256)
}
