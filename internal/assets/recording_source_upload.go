package assets

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const RecordingSourceRetention = 7 * 24 * time.Hour

// Sources are private recording inputs, never ordinary assets or scan jobs.
// The durable finalizing row is the receipt permitting the browser to close.
type RecordingSource struct {
	ID             string     `json:"sourceId"`
	OwnerService   string     `json:"-"`
	ActorID        string     `json:"-"`
	RecordingID    string     `json:"recordingId"`
	IdempotencyKey string     `json:"-"`
	FileName       string     `json:"fileName"`
	SizeBytes      int64      `json:"sizeBytes"`
	ChecksumSHA256 string     `json:"checksumSHA256"`
	BlockCount     int        `json:"blockCount"`
	State          string     `json:"state"`
	CreatedAt      time.Time  `json:"createdAt"`
	ExpiresAt      time.Time  `json:"expiresAt"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
	RetryUntil     *time.Time `json:"retryUntil,omitempty"`
	StagingETag    string     `json:"-"`
}

type CreateRecordingSourceInput struct {
	ActorID        string `json:"-"`
	IdempotencyKey string `json:"-"`
	RecordingID    string `json:"recordingId"`
	FileName       string `json:"fileName"`
	SizeBytes      int64  `json:"sizeBytes"`
	ChecksumSHA256 string `json:"checksumSHA256"`
}

type SignedRecordingSourceBlock struct {
	Number int `json:"number"`
	UploadTarget
}

type RecordingSourceStatus struct {
	RecordingSource
	ConfirmedBlocks []int `json:"confirmedBlocks"`
	NextCursor      int   `json:"nextCursor"`
}

type RecordingSourceRepository interface {
	WithSessionLock(context.Context, string, func(context.Context) error) error
	FindByIdempotency(context.Context, string) (RecordingSource, error)
	Create(context.Context, RecordingSource) error
	Get(context.Context, string) (RecordingSource, error)
	Finalize(context.Context, string, string, time.Time) error
}

type RecordingSourceObjectStore interface {
	SignRecordingSourceBlock(context.Context, string, int, time.Time) (UploadTarget, error)
	CommitRecordingSource(context.Context, string, int64) (BlobMetadata, error)
	RecordingSourceBlocks(context.Context, string) (RecordingSourceBlockList, error)
}

func (s *RecordingSourceService) Status(ctx context.Context, id, actor string, cursor, limit int) (RecordingSourceStatus, error) {
	p, err := s.Get(ctx, id, actor)
	if err != nil {
		return RecordingSourceStatus{}, err
	}
	if limit < 1 || limit > 1000 || cursor < 0 || cursor > p.BlockCount {
		return RecordingSourceStatus{}, ErrInvalidInput
	}
	page := RecordingSourceStatus{RecordingSource: p, ConfirmedBlocks: []int{}}
	if p.State != "uploading" {
		return page, nil
	}
	if !s.now().Before(p.ExpiresAt) {
		page.State = "expired"
		return page, nil
	}
	blocks, err := s.objects.RecordingSourceBlocks(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return page, nil
	}
	if err != nil {
		return RecordingSourceStatus{}, err
	}
	confirmed, err := ConfirmedRecordingSourceBlocks(p.SizeBytes, blocks.Committed, blocks.Uncommitted)
	if err != nil {
		return RecordingSourceStatus{}, err
	}
	for _, number := range confirmed {
		if number <= cursor {
			continue
		}
		if len(page.ConfirmedBlocks) == limit {
			page.NextCursor = page.ConfirmedBlocks[len(page.ConfirmedBlocks)-1]
			break
		}
		page.ConfirmedBlocks = append(page.ConfirmedBlocks, number)
	}
	return page, nil
}

type RecordingSourceService struct {
	repository RecordingSourceRepository
	objects    RecordingSourceObjectStore
	now        func() time.Time
}

func NewRecordingSourceService(repository RecordingSourceRepository, objects RecordingSourceObjectStore, now func() time.Time) *RecordingSourceService {
	return &RecordingSourceService{repository: repository, objects: objects, now: now}
}

func (s *RecordingSourceService) Create(ctx context.Context, input CreateRecordingSourceInput) (RecordingSource, error) {
	count, err := RecordingSourceBlockCount(input.SizeBytes)
	if err != nil {
		return RecordingSource{}, err
	}
	input.FileName = strings.TrimSpace(input.FileName)
	input.ChecksumSHA256 = strings.ToLower(input.ChecksumSHA256)
	if !mediaID.MatchString(input.ActorID) || !mediaID.MatchString(input.RecordingID) || strings.TrimSpace(input.IdempotencyKey) == "" || len(input.IdempotencyKey) > 128 || !utf8.ValidString(input.FileName) || len(input.FileName) > 255 || len(input.FileName) <= 4 || !strings.HasSuffix(strings.ToLower(input.FileName), ".mp4") || strings.ContainsAny(input.FileName, "/\\") || strings.ContainsFunc(input.FileName, unicode.IsControl) || len(input.ChecksumSHA256) != 64 {
		return RecordingSource{}, ErrInvalidInput
	}
	if _, err := hex.DecodeString(input.ChecksumSHA256); err != nil {
		return RecordingSource{}, ErrInvalidInput
	}
	if p, err := s.repository.FindByIdempotency(ctx, input.IdempotencyKey); err == nil {
		if p.OwnerService != "hhc-web-api" || p.ActorID != input.ActorID || p.RecordingID != input.RecordingID || p.FileName != input.FileName || p.SizeBytes != input.SizeBytes || p.ChecksumSHA256 != input.ChecksumSHA256 || p.State == "expired" || (p.State == "uploading" && !s.now().Before(p.ExpiresAt)) {
			return RecordingSource{}, ErrConflict
		}
		return p, nil
	} else if !errors.Is(err, ErrNotFound) {
		return RecordingSource{}, err
	}
	now := s.now().UTC()
	p := RecordingSource{ID: newID(), OwnerService: "hhc-web-api", ActorID: input.ActorID, RecordingID: input.RecordingID, IdempotencyKey: input.IdempotencyKey, FileName: input.FileName, SizeBytes: input.SizeBytes, ChecksumSHA256: input.ChecksumSHA256, BlockCount: count, State: "uploading", CreatedAt: now, ExpiresAt: now.Add(RecordingUploadTTL)}
	if err := s.repository.Create(ctx, p); err != nil {
		return RecordingSource{}, err
	}
	return p, nil
}

func (s *RecordingSourceService) Get(ctx context.Context, id, actor string) (RecordingSource, error) {
	if !mediaID.MatchString(id) || !mediaID.MatchString(actor) {
		return RecordingSource{}, ErrInvalidInput
	}
	p, err := s.repository.Get(ctx, id)
	if err != nil {
		return RecordingSource{}, err
	}
	if p.OwnerService != "hhc-web-api" || p.ActorID != actor {
		return RecordingSource{}, ErrForbidden
	}
	return p, nil
}

func (s *RecordingSourceService) Sign(ctx context.Context, id, actor string, numbers []int) ([]SignedRecordingSourceBlock, error) {
	if len(numbers) < 1 || len(numbers) > 100 {
		return nil, ErrInvalidInput
	}
	var signed []SignedRecordingSourceBlock
	err := s.repository.WithSessionLock(ctx, id, func(ctx context.Context) error {
		p, err := s.Get(ctx, id, actor)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		if p.State != "uploading" || !now.Before(p.ExpiresAt) {
			return ErrConflict
		}
		seen := make(map[int]bool, len(numbers))
		for _, number := range numbers {
			if number < 1 || number > p.BlockCount || seen[number] {
				return ErrInvalidInput
			}
			seen[number] = true
		}
		at := now.Add(RecordingPartURLTTL)
		if p.ExpiresAt.Before(at) {
			at = p.ExpiresAt
		}
		for _, number := range numbers {
			target, err := s.objects.SignRecordingSourceBlock(ctx, id, number, at)
			if err != nil {
				return err
			}
			signed = append(signed, SignedRecordingSourceBlock{Number: number, UploadTarget: target})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return signed, nil
}

func (s *RecordingSourceService) Complete(ctx context.Context, id, actor string) (RecordingSource, error) {
	var p RecordingSource
	err := s.repository.WithSessionLock(ctx, id, func(ctx context.Context) error {
		var err error
		p, err = s.Get(ctx, id, actor)
		if err != nil {
			return err
		}
		switch p.State {
		case "finalizing", "queued", "processing", "ready", "failed":
			// Lost-response replays never recommit bytes or extend the retry window.
			return nil
		case "uploading":
			if !s.now().Before(p.ExpiresAt) {
				return ErrConflict
			}
		default:
			return ErrConflict
		}
		metadata, err := s.objects.CommitRecordingSource(ctx, id, p.SizeBytes)
		if err != nil {
			return err
		}
		if metadata.Size != p.SizeBytes || metadata.ETag == "" {
			return ErrInvalidUpload
		}
		if err := s.repository.Finalize(ctx, id, metadata.ETag, s.now().UTC()); err != nil {
			return err
		}
		p, err = s.repository.Get(ctx, id)
		return err
	})
	return p, err
}
