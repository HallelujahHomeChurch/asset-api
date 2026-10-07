package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	ErrCaptureMissingObjects = errors.New("capture_missing_objects")
	ErrCaptureExpired        = errors.New("capture_expired")
	captureUUID              = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
	captureOperationKey      = regexp.MustCompile(`^[a-zA-Z0-9._:-]{1,128}$`)
	captureObjectPath        = regexp.MustCompile(`^(master\.m3u8|(480p|720p|1080p)/(index\.m3u8|init\.mp4|seg-[0-9]{6}\.m4s))$`)
)

type RecordingCaptureObject struct {
	RecordingPackageObject
	State string `json:"state"`
}
type RecordingCaptureReceipt struct {
	OperationKey string    `json:"operationKey"`
	Operation    string    `json:"operation"`
	AcceptedAt   time.Time `json:"acceptedAt"`
	CaptureID    string    `json:"captureId"`
}
type RecordingCaptureStoredReceipt struct {
	Digest  string
	Receipt RecordingCaptureReceipt
}

// Receipts and declarations are persisted together. Queued is upload evidence,
// never immutable hash/media validation or an authorization to play.
type RecordingCapture struct {
	ID, ActorID, RecordingID, CreateKey, State string
	CreatedAt, ExpiresAt                       time.Time
	DeclaredBytes                              int64
	DeclaredObjects                            int
	Objects                                    []RecordingCaptureObject
	Receipts                                   map[string]RecordingCaptureStoredReceipt
	PackageID                                  *string
	TerminalAt                                 *time.Time
	TerminalReason                             *string
	Inventory                                  *RecordingPackageInventory
}
type RecordingLiveProgress struct {
	Revision        int64      `json:"revision"`
	FirstSequence   int        `json:"firstSequence"`
	LastSequence    int        `json:"lastSequence"`
	MediaEndSeconds float64    `json:"mediaEndSeconds"`
	LastAdvancedAt  *time.Time `json:"lastAdvancedAt"`
	EndedAt         *time.Time `json:"endedAt"`
	Ended           bool       `json:"ended"`
}
type RecordingCaptureStatus struct {
	ID              string                   `json:"captureId"`
	RecordingID     string                   `json:"recordingId"`
	State           string                   `json:"state"`
	CreatedAt       time.Time                `json:"createdAt"`
	ExpiresAt       time.Time                `json:"expiresAt"`
	DeclaredBytes   int64                    `json:"declaredBytes"`
	DeclaredObjects int                      `json:"declaredObjects"`
	Objects         []RecordingCaptureObject `json:"objects"`
	NextCursor      string                   `json:"nextCursor"`
	PackageID       *string                  `json:"packageId"`
	TerminalAt      *time.Time               `json:"terminalAt"`
	TerminalReason  *string                  `json:"terminalReason"`
	Progress        RecordingLiveProgress    `json:"progress"`
}
type RecordingCaptureResult struct {
	Capture RecordingCaptureStatus  `json:"capture"`
	Receipt RecordingCaptureReceipt `json:"receipt"`
}
type RecordingCaptureRepository interface {
	CreateCapture(context.Context, RecordingCapture) (RecordingCapture, error)
	GetCapture(context.Context, string) (RecordingCapture, error)
	// Callback and declarations/receipt/package insertion commit atomically.
	UpdateCapture(context.Context, string, func(*RecordingCapture) error) (RecordingCapture, error)
}
type RecordingCaptureService struct {
	repository RecordingCaptureRepository
	objects    RecordingPackageObjectStore
	now        func() time.Time
}

func NewRecordingCaptureService(repository RecordingCaptureRepository, objects RecordingPackageObjectStore, now func() time.Time) *RecordingCaptureService {
	return &RecordingCaptureService{repository, objects, now}
}
func captureStatus(c RecordingCapture, cursor string, limit int) (RecordingCaptureStatus, error) {
	objects := slices.Clone(c.Objects)
	slices.SortFunc(objects, func(a, b RecordingCaptureObject) int { return strings.Compare(a.Path, b.Path) })
	start := 0
	if cursor != "" {
		i := slices.IndexFunc(objects, func(o RecordingCaptureObject) bool { return o.Path == cursor })
		if i < 0 {
			return RecordingCaptureStatus{}, ErrInvalidInput
		}
		start = i + 1
	}
	end := min(start+limit, len(objects))
	page := RecordingCaptureStatus{ID: c.ID, RecordingID: c.RecordingID, State: c.State, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt, DeclaredBytes: c.DeclaredBytes, DeclaredObjects: c.DeclaredObjects, Objects: append([]RecordingCaptureObject{}, objects[start:end]...), PackageID: c.PackageID, TerminalAt: c.TerminalAt, TerminalReason: c.TerminalReason, Progress: RecordingLiveProgress{LastSequence: -1}}
	if end < len(objects) {
		page.NextCursor = objects[end-1].Path
	}
	return page, nil
}
func captureOwner(c RecordingCapture, actor string) error {
	if actor == "" || c.ActorID != actor {
		return ErrForbidden
	}
	return nil
}
func (s *RecordingCaptureService) Create(ctx context.Context, recording, actor, key string) (RecordingCaptureResult, error) {
	if !captureUUID.MatchString(recording) || !captureUUID.MatchString(actor) || !captureOperationKey.MatchString(key) {
		return RecordingCaptureResult{}, ErrInvalidInput
	}
	now := s.now().UTC()
	c := RecordingCapture{ID: newID(), RecordingID: recording, ActorID: actor, CreateKey: key, State: "uploading", CreatedAt: now, ExpiresAt: now.Add(RecordingUploadTTL), Receipts: map[string]RecordingCaptureStoredReceipt{}}
	digest := captureDigest([]string{recording, actor})
	c.Receipts[key] = RecordingCaptureStoredReceipt{digest, RecordingCaptureReceipt{key, "create", now, c.ID}}
	got, err := s.repository.CreateCapture(ctx, c)
	if err != nil {
		return RecordingCaptureResult{}, err
	}
	if got.ActorID != actor || got.RecordingID != recording || got.Receipts[key].Digest != digest {
		return RecordingCaptureResult{}, ErrConflict
	}
	page, _ := captureStatus(got, "", 100)
	return RecordingCaptureResult{page, got.Receipts[key].Receipt}, nil
}
func captureDigest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func (s *RecordingCaptureService) Get(ctx context.Context, id, actor, cursor string, limit int) (RecordingCaptureStatus, error) {
	if limit < 1 || limit > 1000 {
		return RecordingCaptureStatus{}, ErrInvalidInput
	}
	c, err := s.repository.GetCapture(ctx, id)
	if err != nil {
		return RecordingCaptureStatus{}, err
	}
	if err = captureOwner(c, actor); err != nil {
		return RecordingCaptureStatus{}, err
	}
	if c.State == "uploading" && !s.now().Before(c.ExpiresAt) {
		c.State = "expired"
		at := c.ExpiresAt
		c.TerminalAt = &at
		reason := "capture_expired"
		c.TerminalReason = &reason
	}
	return captureStatus(c, cursor, limit)
}
func (s *RecordingCaptureService) mutate(ctx context.Context, id, actor, key, operation string, input any, fn func(*RecordingCapture) error) (RecordingCaptureResult, error) {
	if !captureOperationKey.MatchString(key) {
		return RecordingCaptureResult{}, ErrInvalidInput
	}
	digest := captureDigest(input)
	c, err := s.repository.UpdateCapture(ctx, id, func(c *RecordingCapture) error {
		if err := captureOwner(*c, actor); err != nil {
			return err
		}
		if old, ok := c.Receipts[key]; ok {
			if old.Digest != digest || old.Receipt.Operation != operation {
				return ErrConflict
			}
			return nil
		}
		if c.State == "expired" {
			return ErrCaptureExpired
		}
		if c.State != "uploading" && !(operation == "abort" && (c.State == "freezing" || c.State == "validating")) {
			return ErrConflict
		}
		if !s.now().Before(c.ExpiresAt) {
			return ErrCaptureExpired
		}
		if len(c.Receipts) >= 20002 {
			return ErrConflict
		}
		if err := fn(c); err != nil {
			return err
		}
		c.Receipts[key] = RecordingCaptureStoredReceipt{digest, RecordingCaptureReceipt{key, operation, s.now().UTC(), c.ID}}
		return nil
	})
	if err != nil {
		return RecordingCaptureResult{}, err
	}
	page, _ := captureStatus(c, "", 100)
	return RecordingCaptureResult{page, c.Receipts[key].Receipt}, nil
}
func validCaptureObject(o RecordingPackageObject) bool {
	hash, err := hex.DecodeString(o.SHA256)
	return captureObjectPath.MatchString(o.Path) && o.SizeBytes > 0 && o.SizeBytes <= RecordingObjectMaxBytes && (!strings.HasSuffix(o.Path, ".m3u8") || o.SizeBytes <= RecordingPlaylistMaxBytes) && err == nil && len(hash) == 32 && strings.ToLower(o.SHA256) == o.SHA256
}
func (s *RecordingCaptureService) Declare(ctx context.Context, id, actor, key string, objects []RecordingPackageObject) (RecordingCaptureResult, error) {
	if len(objects) < 1 || len(objects) > 100 {
		return RecordingCaptureResult{}, ErrInvalidInput
	}
	objects = slices.Clone(objects)
	slices.SortFunc(objects, func(a, b RecordingPackageObject) int { return strings.Compare(a.Path, b.Path) })
	for i, o := range objects {
		if !validCaptureObject(o) || (i > 0 && objects[i-1].Path == o.Path) {
			return RecordingCaptureResult{}, ErrInvalidInput
		}
	}
	return s.mutate(ctx, id, actor, key, "declare", objects, func(c *RecordingCapture) error {
		existing := map[string]RecordingPackageObject{}
		for _, o := range c.Objects {
			existing[o.Path] = o.RecordingPackageObject
		}
		for _, o := range objects {
			if old, ok := existing[o.Path]; ok {
				if old != o {
					return ErrConflict
				}
				continue
			}
			if c.DeclaredObjects >= RecordingPackageMaxObjects || o.SizeBytes > RecordingPackageMaxBytes-c.DeclaredBytes {
				return ErrRecordingPackageTooLarge
			}
			c.Objects = append(c.Objects, RecordingCaptureObject{o, "declared"})
			c.DeclaredObjects++
			c.DeclaredBytes += o.SizeBytes
		}
		return nil
	})
}
func capturePaths(c RecordingCapture, paths []string) ([]int, error) {
	if len(paths) < 1 || len(paths) > 100 {
		return nil, ErrInvalidInput
	}
	index := map[string]int{}
	for i, o := range c.Objects {
		index[o.Path] = i
	}
	seen := map[string]bool{}
	indices := []int{}
	for _, p := range paths {
		i, ok := index[p]
		if !ok || seen[p] {
			return nil, ErrInvalidInput
		}
		seen[p] = true
		indices = append(indices, i)
	}
	return indices, nil
}
func (s *RecordingCaptureService) Sign(ctx context.Context, id, actor string, paths []string) ([]SignedRecordingObject, error) {
	signed := []SignedRecordingObject{}
	_, err := s.repository.UpdateCapture(ctx, id, func(c *RecordingCapture) error {
		if err := captureOwner(*c, actor); err != nil {
			return err
		}
		if c.State == "expired" {
			return ErrCaptureExpired
		}
		if c.State != "uploading" {
			return ErrConflict
		}
		if !s.now().Before(c.ExpiresAt) {
			return ErrCaptureExpired
		}
		indices, err := capturePaths(*c, paths)
		if err != nil {
			return err
		}
		for _, i := range indices {
			o := c.Objects[i]
			if o.State != "declared" {
				return ErrConflict
			}
			ttl := min(RecordingPartURLTTL, c.ExpiresAt.Sub(s.now())).Truncate(time.Second)
			if ttl <= 0 {
				return ErrCaptureExpired
			}
			url, err := s.objects.PresignPackageObject(ctx, (RecordingPackage{ID: c.ID}).StagingKey(o.Path), o.SizeBytes, recordingObjectContentType(o.Path), ttl)
			if err != nil {
				return err
			}
			signed = append(signed, SignedRecordingObject{Path: o.Path, PresignedPart: url})
		}
		return nil
	})
	return signed, err
}
func (s *RecordingCaptureService) Confirm(ctx context.Context, id, actor, key string, paths []string) (RecordingCaptureResult, error) {
	paths = slices.Clone(paths)
	slices.Sort(paths)
	return s.mutate(ctx, id, actor, key, "confirm", paths, func(c *RecordingCapture) error {
		indices, err := capturePaths(*c, paths)
		if err != nil {
			return err
		}
		sizes, err := s.objects.ListPackageObjects(ctx, c.ID, RecordingPackageMaxObjects)
		if err != nil {
			return err
		}
		for _, i := range indices {
			o := c.Objects[i]
			if o.State == "failed" {
				return ErrConflict
			}
			if sizes[o.Path] != o.SizeBytes {
				return ErrCaptureMissingObjects
			}
		}
		for _, i := range indices {
			if c.Objects[i].State == "declared" {
				c.Objects[i].State = "queued"
			}
		}
		return nil
	})
}
func (s *RecordingCaptureService) Seal(ctx context.Context, id, actor, key string, normalEnd bool, inv RecordingPackageInventory) (RecordingCaptureResult, error) {
	if !normalEnd || len(inv.Renditions) != 3 || inv.InventoryDigest == "" {
		return RecordingCaptureResult{}, ErrInvalidInput
	}
	if _, err := ValidateRecordingInventory(inv); err != nil {
		return RecordingCaptureResult{}, err
	}
	return s.mutate(ctx, id, actor, key, "seal", struct {
		NormalEnd bool
		Digest    string
	}{normalEnd, inv.InventoryDigest}, func(c *RecordingCapture) error {
		if len(inv.Objects) != len(c.Objects) {
			return ErrCaptureMissingObjects
		}
		declared := map[string]RecordingCaptureObject{}
		for _, o := range c.Objects {
			declared[o.Path] = o
		}
		for _, o := range inv.Objects {
			old, ok := declared[o.Path]
			if !ok || old.RecordingPackageObject != o {
				return ErrConflict
			}
			if old.State != "queued" && old.State != "verified" {
				return ErrCaptureMissingObjects
			}
		}
		c.State = "freezing"
		packageID := c.ID
		c.PackageID = &packageID
		c.Inventory = &inv
		return nil
	})
}
func (s *RecordingCaptureService) Abort(ctx context.Context, id, actor, key, reason string) (RecordingCaptureResult, error) {
	switch reason {
	case "user_abort", "disk_limit", "encoder_failure", "capture_incomplete":
	default:
		return RecordingCaptureResult{}, ErrInvalidInput
	}
	return s.mutate(ctx, id, actor, key, "abort", reason, func(c *RecordingCapture) error {
		at := s.now().UTC()
		c.State = "aborted"
		c.TerminalAt = &at
		c.TerminalReason = &reason
		return nil
	})
}
