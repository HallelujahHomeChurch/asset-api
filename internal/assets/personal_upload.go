package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"
)

const PersonalMaxFileSize int64 = 200 << 20

type PersonalUploadInput struct {
	FileName  string `json:"fileName"`
	MIMEType  string `json:"mimeType"`
	SizeBytes int64  `json:"sizeBytes"`
}
type PersonalUploadState struct {
	ID               string           `json:"id"`
	ContentPath      string           `json:"contentPath"`
	ExpiresAt        time.Time        `json:"expiresAt"`
	UploadStatus     UploadStatus     `json:"uploadStatus"`
	ScanStatus       ScanStatus       `json:"scanStatus"`
	ProcessingStatus ProcessingStatus `json:"processingStatus"`
}

func personalUploadState(a Asset, u UploadSession) PersonalUploadState {
	return PersonalUploadState{ID: u.ID, ContentPath: "/api/assets/personal-space/uploads/" + u.ID + "/content", ExpiresAt: u.ExpiresAt.UTC().Truncate(time.Microsecond), UploadStatus: a.UploadStatus, ScanStatus: a.ScanStatus, ProcessingStatus: a.ProcessingStatus}
}
func (s *Service) CreatePersonalUpload(ctx context.Context, owner string, input PersonalUploadInput, key string) (PersonalUploadState, error) {
	if owner == "" {
		return PersonalUploadState{}, ErrUnauthorized
	}
	if strings.TrimSpace(key) == "" || len(key) > 128 || strings.TrimSpace(input.FileName) == "" || len(input.FileName) > 1024 || strings.ContainsAny(input.FileName, "/\\\x00") {
		return PersonalUploadState{}, ErrInvalidInput
	}
	// Scope the existing service-wide idempotency key to the authenticated account.
	hash := sha256.Sum256([]byte(owner + "\x00" + key))
	created, err := s.CreateUploadSession(ctx, CreateUploadInput{Namespace: PersonalNamespace, OwnerService: PersonalNamespace, OwnerType: "user", OwnerID: owner, Purpose: "personal-sync", OriginalFileName: input.FileName, ExpectedMIMEType: input.MIMEType, MaxSizeBytes: input.SizeBytes, Visibility: VisibilityPrivate}, hex.EncodeToString(hash[:]))
	if err != nil {
		return PersonalUploadState{}, err
	}
	return personalUploadState(created.Asset, created.Session), nil
}
func (s *Service) personalUpload(ctx context.Context, owner, id string) (Asset, UploadSession, error) {
	if owner == "" {
		return Asset{}, UploadSession{}, ErrUnauthorized
	}
	repository, ok := s.repository.(interface {
		PersonalUploadAssetID(context.Context, string, string) (string, error)
	})
	if !ok {
		return Asset{}, UploadSession{}, fmt.Errorf("personal upload repository unavailable")
	}
	assetID, err := repository.PersonalUploadAssetID(ctx, owner, id)
	if err != nil {
		return Asset{}, UploadSession{}, err
	}
	a, err := s.repository.GetAsset(ctx, assetID)
	if err != nil {
		return Asset{}, UploadSession{}, err
	}
	if a.OwnerID != owner || a.OwnerService != PersonalNamespace || a.OwnerType != "user" || a.Namespace != PersonalNamespace || !a.DeletedAt.IsZero() {
		return Asset{}, UploadSession{}, ErrNotFound
	}
	u, err := s.repository.GetUploadSession(ctx, assetID)
	if err != nil {
		return Asset{}, UploadSession{}, err
	}
	if u.ID != id {
		return Asset{}, UploadSession{}, ErrNotFound
	}
	return a, u, nil
}
func (s *Service) PersonalUpload(ctx context.Context, owner, id string) (PersonalUploadState, error) {
	a, u, err := s.personalUpload(ctx, owner, id)
	if err != nil {
		return PersonalUploadState{}, err
	}
	return personalUploadState(a, u), nil
}
func (s *Service) PutPersonalUpload(ctx context.Context, owner, id string, size int64, reader io.Reader) error {
	a, u, err := s.personalUpload(ctx, owner, id)
	if err != nil {
		return err
	}
	if u.Status != UploadCreated || !s.now().Before(u.ExpiresAt) {
		return ErrConflict
	}
	if u.MaxSizeBytes <= 0 || u.MaxSizeBytes > PersonalMaxFileSize || (size >= 0 && size != u.MaxSizeBytes) {
		return ErrInvalidInput
	}
	writer, ok := s.blobs.(interface {
		PutOnce(context.Context, string, io.Reader, int64, string) (BlobProperties, error)
	})
	if !ok {
		return fmt.Errorf("immutable upload storage unavailable")
	}
	props, err := writer.PutOnce(ctx, u.StagingObjectKey, &contextReader{ctx: ctx, reader: io.LimitReader(reader, u.MaxSizeBytes+1)}, u.MaxSizeBytes, a.ExpectedMIMEType)
	if err != nil {
		return err
	}
	if props.Size != u.MaxSizeBytes {
		if err := s.rejectUpload(ctx, a, u); err != nil {
			return err
		}
		return ErrInvalidUpload
	}
	// Completion may have removed staging while this request was still writing.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	latest, err := s.repository.GetUploadSession(cleanupCtx, a.ID)
	if err != nil {
		return err
	}
	if latest.Status != UploadCreated {
		if err := s.blobs.Delete(cleanupCtx, u.StagingObjectKey); err != nil {
			return err
		}
		return ErrConflict
	}
	return nil
}
func (s *Service) CompletePersonalUpload(ctx context.Context, owner, id string, input CompleteUploadInput) (PersonalUploadState, error) {
	a, u, err := s.personalUpload(ctx, owner, id)
	if err != nil {
		return PersonalUploadState{}, err
	}
	if input.SizeBytes != u.MaxSizeBytes || input.MIMEType != a.ExpectedMIMEType {
		return PersonalUploadState{}, ErrInvalidUpload
	}
	a, err = s.CompleteUpload(ctx, a.ID, input)
	if err != nil {
		return PersonalUploadState{}, err
	}
	return personalUploadState(a, u), nil
}
