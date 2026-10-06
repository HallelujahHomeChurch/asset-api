package assets

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"hhc/asset-api/internal/storage/r2"
)

type RecordingPackage struct {
	ID                 string                       `json:"packageId"`
	SessionID          string                       `json:"sessionId"`
	OwnerService       string                       `json:"-"`
	ActorID            string                       `json:"-"`
	RecordingID        string                       `json:"recordingId"`
	IdempotencyKey     string                       `json:"-"`
	State              string                       `json:"state"`
	SizeBytes          int64                        `json:"sizeBytes"`
	CreatedAt          time.Time                    `json:"createdAt"`
	ExpiresAt          time.Time                    `json:"expiresAt"`
	Inventory          RecordingPackageInventory    `json:"inventory"`
	FinalPrefix        string                       `json:"-"`
	ReadyAt            *time.Time                   `json:"readyAt,omitempty"`
	MediaExpiresAt     *time.Time                   `json:"mediaExpiresAt,omitempty"`
	UploadedAt         *time.Time                   `json:"uploadedAt,omitempty"`
	RetentionRevision  int64                        `json:"retentionRevision"`
	ProcessingProgress *RecordingProcessingProgress `json:"-"`
}

func (p RecordingPackage) StagingKey(path string) string {
	return "recordings/packages/" + p.ID + "/staging/" + path
}

type CreateRecordingPackageInput struct {
	ActorID        string                    `json:"-"`
	IdempotencyKey string                    `json:"-"`
	RecordingID    string                    `json:"recordingId"`
	Inventory      RecordingPackageInventory `json:"inventory"`
}

type RecordingPackageStatus struct {
	ProcessingProgress *RecordingProcessingProgress `json:"processingProgress,omitempty"`
	PackageID          string                       `json:"packageId"`
	SessionID          string                       `json:"sessionId"`
	RecordingID        string                       `json:"recordingId"`
	State              string                       `json:"state"`
	SizeBytes          int64                        `json:"sizeBytes"`
	ExpiresAt          time.Time                    `json:"expiresAt"`
	ConfirmedObjects   []string                     `json:"confirmedObjects"`
	NextCursor         string                       `json:"nextCursor"`
	ReadyAt            *time.Time                   `json:"readyAt,omitempty"`
	MediaExpiresAt     *time.Time                   `json:"mediaExpiresAt,omitempty"`
	Renditions         []RecordingRendition         `json:"renditions,omitempty"`
	UploadedAt         *time.Time                   `json:"uploadedAt,omitempty"`
	RetentionRevision  int64                        `json:"retentionRevision"`
}

type SignedRecordingObject struct {
	Path string `json:"path"`
	r2.PresignedPart
}

type RecordingPackageRepository interface {
	WithSessionLock(context.Context, string, func(context.Context) error) error
	FindByIdempotency(context.Context, string) (RecordingPackage, error)
	Create(context.Context, RecordingPackage) error
	Get(context.Context, string) (RecordingPackage, error)
	Freeze(context.Context, string, time.Time) error
}

type RecordingPackageObjectStore interface {
	PresignPackageObject(context.Context, string, int64, string, time.Duration) (r2.PresignedPart, error)
	ListPackageObjects(context.Context, string, int) (map[string]int64, error)
}

type RecordingPackageService struct {
	repository RecordingPackageRepository
	objects    RecordingPackageObjectStore
	now        func() time.Time
}

func NewRecordingPackageService(repository RecordingPackageRepository, objects RecordingPackageObjectStore, now func() time.Time) *RecordingPackageService {
	return &RecordingPackageService{repository: repository, objects: objects, now: now}
}

func (s *RecordingPackageService) Create(ctx context.Context, input CreateRecordingPackageInput) (RecordingPackage, error) {
	if !mediaID.MatchString(input.ActorID) || !mediaID.MatchString(input.RecordingID) || strings.TrimSpace(input.IdempotencyKey) == "" || len(input.IdempotencyKey) > 128 || input.Inventory.InventoryDigest == "" {
		return RecordingPackage{}, ErrInvalidInput
	}
	size, err := ValidateRecordingInventory(input.Inventory)
	if err != nil {
		return RecordingPackage{}, err
	}
	if p, err := s.repository.FindByIdempotency(ctx, input.IdempotencyKey); err == nil {
		if p.OwnerService != "hhc-web-api" || p.ActorID != input.ActorID || p.RecordingID != input.RecordingID || p.Inventory.InventoryDigest != input.Inventory.InventoryDigest || p.State == "failed" || p.State == "expired" || (p.State == "uploading" && !s.now().Before(p.ExpiresAt)) {
			return RecordingPackage{}, ErrConflict
		}
		return p, nil
	} else if !errors.Is(err, ErrNotFound) {
		return RecordingPackage{}, err
	}
	inv := input.Inventory
	inv.Objects = slices.Clone(inv.Objects)
	inv.Renditions = slices.Clone(inv.Renditions)
	slices.SortFunc(inv.Objects, func(a, b RecordingPackageObject) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(inv.Renditions, func(a, b RecordingRendition) int { return strings.Compare(a.Name, b.Name) })
	now := s.now().UTC()
	id := newID()
	p := RecordingPackage{ID: id, SessionID: id, OwnerService: "hhc-web-api", ActorID: input.ActorID, RecordingID: input.RecordingID, IdempotencyKey: input.IdempotencyKey, State: "uploading", SizeBytes: size, CreatedAt: now, ExpiresAt: now.Add(RecordingUploadTTL), Inventory: inv}
	if err := s.repository.Create(ctx, p); err != nil {
		return RecordingPackage{}, err
	}
	return p, nil
}

func (s *RecordingPackageService) Get(ctx context.Context, id, actor string) (RecordingPackage, error) {
	p, err := s.repository.Get(ctx, id)
	if err != nil {
		return RecordingPackage{}, err
	}
	if actor == "" || p.ActorID != actor || p.OwnerService != "hhc-web-api" {
		return RecordingPackage{}, ErrForbidden
	}
	return p, nil
}

// Only the owning CMS may use this private operation after checking the viewer's
// entitlement/publication. Uploader identity is not the viewer identity.
func (s *RecordingPackageService) GetReady(ctx context.Context, id, recordingID string) (RecordingPackage, error) {
	p, err := s.repository.Get(ctx, id)
	if err != nil {
		return RecordingPackage{}, err
	}
	if p.OwnerService != "hhc-web-api" || p.RecordingID != recordingID || p.State != "ready" || p.ReadyAt == nil || p.MediaExpiresAt == nil || p.FinalPrefix == "" || !s.now().Before(*p.MediaExpiresAt) {
		return RecordingPackage{}, ErrForbidden
	}
	return p, nil
}

// The callback must issue the grant while holding the policy/package lock.
func (s *RecordingPackageService) WithReady(ctx context.Context, id, recordingID string, use func(RecordingPackage) error) error {
	return s.repository.WithSessionLock(ctx, id, func(ctx context.Context) error {
		p, err := s.GetReady(ctx, id, recordingID)
		if err != nil {
			return err
		}
		return use(p)
	})
}

func (s *RecordingPackageService) Sign(ctx context.Context, id, actor string, paths []string) ([]SignedRecordingObject, error) {
	if len(paths) < 1 || len(paths) > 100 {
		return nil, ErrInvalidInput
	}
	var signed []SignedRecordingObject
	err := s.repository.WithSessionLock(ctx, id, func(ctx context.Context) error {
		p, err := s.Get(ctx, id, actor)
		if err != nil {
			return err
		}
		now := s.now()
		if p.State != "uploading" || !now.Before(p.ExpiresAt) {
			return ErrConflict
		}
		objects := make(map[string]RecordingPackageObject, len(p.Inventory.Objects))
		for _, object := range p.Inventory.Objects {
			objects[object.Path] = object
		}
		seen := make(map[string]bool, len(paths))
		for _, path := range paths {
			if _, ok := objects[path]; !ok || seen[path] {
				return ErrInvalidInput
			}
			seen[path] = true
		}
		for _, path := range paths {
			ttl := min(RecordingPartURLTTL, p.ExpiresAt.Sub(s.now())).Truncate(time.Second)
			if ttl <= 0 {
				return ErrConflict
			}
			object := objects[path]
			url, err := s.objects.PresignPackageObject(ctx, p.StagingKey(path), object.SizeBytes, recordingObjectContentType(path), ttl)
			if err != nil {
				return err
			}
			signed = append(signed, SignedRecordingObject{Path: path, PresignedPart: url})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return signed, nil
}

func recordingObjectContentType(path string) string {
	if strings.HasSuffix(path, ".m3u8") {
		return "application/vnd.apple.mpegurl"
	}
	return "video/mp4"
}

// Complete durably queues freezing. It performs no long copy/hash operation in
// the request, and never claims that bytes or media have already been verified.
func (s *RecordingPackageService) Complete(ctx context.Context, id, actor string) (RecordingPackage, error) {
	var p RecordingPackage
	err := s.repository.WithSessionLock(ctx, id, func(ctx context.Context) error {
		var err error
		p, err = s.Get(ctx, id, actor)
		if err != nil {
			return err
		}
		switch p.State {
		case "freezing", "validating", "ready":
			return nil
		case "uploading":
			if err := s.repository.Freeze(ctx, id, s.now().UTC()); err != nil {
				return err
			}
			p.State = "freezing"
			return nil
		default:
			return ErrConflict
		}
	})
	return p, err
}

// Status confirms remote object sizes for upload/resume, not media validity.
// Final hash, closure and codec validation must succeed before state=ready.
func (s *RecordingPackageService) Status(ctx context.Context, id, actor, cursor string, limit int) (RecordingPackageStatus, error) {
	if limit < 1 || limit > 1000 {
		return RecordingPackageStatus{}, ErrInvalidInput
	}
	p, err := s.Get(ctx, id, actor)
	if err != nil {
		return RecordingPackageStatus{}, err
	}
	objects := slices.Clone(p.Inventory.Objects)
	slices.SortFunc(objects, func(a, b RecordingPackageObject) int { return strings.Compare(a.Path, b.Path) })
	start := 0
	if cursor != "" {
		index := slices.IndexFunc(objects, func(o RecordingPackageObject) bool { return o.Path == cursor })
		if index < 0 {
			return RecordingPackageStatus{}, ErrInvalidInput
		}
		start = index + 1
	}
	end := min(start+limit, len(objects))
	page := RecordingPackageStatus{ProcessingProgress: p.ProcessingProgress, PackageID: p.ID, SessionID: p.SessionID, RecordingID: p.RecordingID, State: p.State, SizeBytes: p.SizeBytes, ExpiresAt: p.ExpiresAt, ConfirmedObjects: []string{}}
	page.ReadyAt, page.MediaExpiresAt = p.ReadyAt, p.MediaExpiresAt
	page.UploadedAt, page.RetentionRevision = p.UploadedAt, p.RetentionRevision
	if p.State == "ready" {
		page.Renditions = slices.Clone(p.Inventory.Renditions)
		if p.MediaExpiresAt != nil && !s.now().Before(*p.MediaExpiresAt) {
			page.State = "expired"
		}
	}
	if end < len(objects) {
		page.NextCursor = objects[end-1].Path
	}
	if start == end || p.State != "uploading" || !s.now().Before(p.ExpiresAt) {
		return page, nil
	}
	// R2's strongly consistent listing supplies the same remote size evidence
	// without one serial network round-trip per HLS fragment. Hash/media checks
	// remain the validation worker's responsibility, never inferred from status.
	sizes, err := s.objects.ListPackageObjects(ctx, p.ID, RecordingPackageMaxObjects)
	if err != nil {
		return RecordingPackageStatus{}, err
	}
	for _, object := range objects[start:end] {
		if size, exists := sizes[object.Path]; exists && size == object.SizeBytes {
			page.ConfirmedObjects = append(page.ConfirmedObjects, object.Path)
		}
	}
	return page, nil
}
