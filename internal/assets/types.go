package assets

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrInvalidInput         = errors.New("invalid input")
	ErrInvalidUpload        = errors.New("invalid upload")
	ErrNotFound             = errors.New("not found")
	ErrForbidden            = errors.New("forbidden")
	ErrConflict             = errors.New("conflict")
	ErrCommitOutcomeUnknown = errors.New("commit outcome unknown")
)

type Visibility string

const (
	VisibilityPublic        Visibility = "public"
	VisibilityAuthenticated Visibility = "authenticated"
	VisibilityRestricted    Visibility = "restricted"
	VisibilityPrivate       Visibility = "private"
)

type UploadStatus string

const (
	UploadCreated   UploadStatus = "created"
	UploadCompleted UploadStatus = "completed"
	UploadFailed    UploadStatus = "failed"
)

type ScanStatus string

const (
	ScanPending  ScanStatus = "pending"
	ScanClean    ScanStatus = "clean"
	ScanInfected ScanStatus = "infected"
	ScanFailed   ScanStatus = "failed"
)

type ProcessingStatus string

const (
	ProcessingPending     ProcessingStatus = "pending"
	ProcessingReady       ProcessingStatus = "ready"
	ProcessingNotRequired ProcessingStatus = "not_required"
	ProcessingFailed      ProcessingStatus = "failed"
)

type SubjectType string

const (
	SubjectPublic    SubjectType = "public"
	SubjectUser      SubjectType = "user"
	SubjectRole      SubjectType = "role"
	SubjectService   SubjectType = "service"
	SubjectLineGroup SubjectType = "line_group"
	SubjectAppClient SubjectType = "app_client"
)

type Permission string

const (
	PermissionRead   Permission = "read"
	PermissionDelete Permission = "delete"
)

type Asset struct {
	ID                 string           `json:"id"`
	Namespace          string           `json:"namespace"`
	OwnerService       string           `json:"ownerService"`
	OwnerType          string           `json:"ownerType"`
	OwnerID            string           `json:"ownerId"`
	Purpose            string           `json:"purpose"`
	Locale             string           `json:"locale,omitempty"`
	OriginalFileName   string           `json:"originalFileName"`
	ObjectKey          string           `json:"-"`
	ExpectedMIMEType   string           `json:"expectedMimeType"`
	DetectedMIMEType   string           `json:"detectedMimeType,omitempty"`
	SizeBytes          int64            `json:"sizeBytes,omitempty"`
	ChecksumSHA256     string           `json:"checksumSha256,omitempty"`
	ETag               string           `json:"etag,omitempty"`
	UploadStatus       UploadStatus     `json:"uploadStatus"`
	ScanStatus         ScanStatus       `json:"scanStatus"`
	ScanDetails        string           `json:"scanDetails,omitempty"`
	ProcessingStatus   ProcessingStatus `json:"processingStatus"`
	Visibility         Visibility       `json:"visibility"`
	CreatedAt          time.Time        `json:"createdAt"`
	UpdatedAt          time.Time        `json:"updatedAt"`
	DeletedAt          time.Time        `json:"deletedAt,omitempty"`
	ScanAttempts       int              `json:"-"`
	ProcessingAttempts int              `json:"-"`
}

type Derivative struct {
	AssetID   string    `json:"assetId"`
	Variant   string    `json:"variant"`
	ObjectKey string    `json:"-"`
	MIMEType  string    `json:"mimeType"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	SizeBytes int64     `json:"sizeBytes"`
	ETag      string    `json:"etag"`
	CreatedAt time.Time `json:"createdAt"`
}

type UploadSession struct {
	ID               string       `json:"id"`
	AssetID          string       `json:"assetId"`
	IdempotencyKey   string       `json:"-"`
	CallerService    string       `json:"-"`
	Operation        string       `json:"-"`
	Fingerprint      string       `json:"-"`
	StagingObjectKey string       `json:"-"`
	MaxSizeBytes     int64        `json:"maxSizeBytes"`
	Status           UploadStatus `json:"status"`
	ExpiresAt        time.Time    `json:"expiresAt"`
	CreatedAt        time.Time    `json:"createdAt"`
	CompletedAt      time.Time    `json:"completedAt,omitempty"`
}

type Grant struct {
	ID             string      `json:"id"`
	AssetID        string      `json:"assetId"`
	SubjectType    SubjectType `json:"subjectType"`
	SubjectID      string      `json:"subjectId"`
	Permission     Permission  `json:"permission"`
	IdempotencyKey string      `json:"-"`
	CallerService  string      `json:"-"`
	Operation      string      `json:"-"`
	Fingerprint    string      `json:"-"`
	ExpiresAt      time.Time   `json:"expiresAt,omitempty"`
	CreatedAt      time.Time   `json:"createdAt"`
	RevokedAt      time.Time   `json:"revokedAt,omitempty"`
}

type CreateUploadInput struct {
	Namespace        string     `json:"namespace"`
	OwnerService     string     `json:"ownerService"`
	OwnerType        string     `json:"ownerType"`
	OwnerID          string     `json:"ownerId"`
	Purpose          string     `json:"purpose"`
	Locale           string     `json:"locale"`
	OriginalFileName string     `json:"originalFileName"`
	ExpectedMIMEType string     `json:"expectedMimeType"`
	MaxSizeBytes     int64      `json:"maxSizeBytes"`
	Visibility       Visibility `json:"visibility"`
}

type CompleteUploadInput struct {
	SizeBytes      int64  `json:"sizeBytes"`
	ChecksumSHA256 string `json:"checksumSha256"`
	MIMEType       string `json:"mimeType"`
}

type ScanRequest struct {
	EventID   string
	AssetID   string
	ETag      string
	Attempts  int
	CreatedAt time.Time
}

type CreateGrantInput struct {
	SubjectType    SubjectType `json:"subjectType"`
	SubjectID      string      `json:"subjectId"`
	Permission     Permission  `json:"permission"`
	ExpiresAt      time.Time   `json:"expiresAt"`
	IdempotencyKey string      `json:"idempotencyKey"`
}

type ScanResult struct {
	EventID         string     `json:"eventId"`
	AssetID         string     `json:"assetId"`
	Status          ScanStatus `json:"status"`
	Details         string     `json:"details,omitempty"`
	ETag            string     `json:"etag,omitempty"`
	ExpectedAttempt int        `json:"-"`
}

type UploadTarget struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expiresAt"`
}

type CreatedUpload struct {
	Asset   Asset         `json:"asset"`
	Session UploadSession `json:"session"`
	Target  UploadTarget  `json:"uploadTarget"`
}

type BlobProperties struct {
	Size             int64
	DetectedMIMEType string
	ChecksumSHA256   string
	ETag             string
}

type BlobMetadata struct {
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
}

type ByteRange struct {
	Offset int64
	Count  int64
	Suffix int64
}
type BlobDownload struct {
	Body         io.ReadCloser
	Size         int64
	TotalSize    int64
	ContentType  string
	ETag         string
	LastModified time.Time
	CacheControl string
}

type PublicDownloadMetadata struct {
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
	CacheControl string
	objectKey    string
}

type Operations struct {
	ScanPending             int64     `json:"scanPending"`
	ScanFailed              int64     `json:"scanFailed"`
	OldestScanPending       time.Time `json:"oldestScanPending,omitempty"`
	ProcessingPending       int64     `json:"processingPending"`
	ProcessingFailed        int64     `json:"processingFailed"`
	OldestProcessingPending time.Time `json:"oldestProcessingPending,omitempty"`
	PurgePending            int64     `json:"purgePending"`
}

type Repository interface {
	CreateUpload(context.Context, Asset, UploadSession) error
	GetAsset(context.Context, string) (Asset, error)
	GetUploadSession(context.Context, string) (UploadSession, error)
	CompleteUpload(context.Context, Asset, UploadSession, ScanRequest) error
	FailUpload(context.Context, string, time.Time) error
	CreateGrant(context.Context, Grant) (Grant, error)
	RevokeGrant(context.Context, string, string, time.Time) error
	HasActiveGrant(context.Context, string, SubjectType, string, Permission, time.Time) (bool, error)
	ApplyScanResult(context.Context, ScanResult, time.Time) (bool, error)
	ClaimPendingScan(context.Context, time.Time, time.Duration) (Asset, bool, error)
	ScheduleScanRetry(context.Context, string, int, string, time.Time, time.Time) error
}

type BlobStore interface {
	CreateUploadTarget(context.Context, string, int64, time.Time) (UploadTarget, error)
	InspectProperties(context.Context, string) (BlobMetadata, error)
	Inspect(context.Context, string, string, int64) (BlobProperties, error)
	Commit(context.Context, string, string) (BlobProperties, error)
	Open(context.Context, string, ByteRange, string) (BlobDownload, error)
	Delete(context.Context, string) error
}
