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
	ErrUnauthorized         = errors.New("unauthorized")
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
	ScanSignature      string           `json:"scanSignatureVersion,omitempty"`
	ScanFailure        string           `json:"scanFailureCategory,omitempty"`
	ProcessingStatus   ProcessingStatus `json:"processingStatus"`
	Visibility         Visibility       `json:"visibility"`
	CreatedAt          time.Time        `json:"createdAt"`
	UpdatedAt          time.Time        `json:"updatedAt"`
	DeletedAt          time.Time        `json:"deletedAt,omitempty"`
	ScanAttempts       int              `json:"-"`
	ScanEventID        string           `json:"-"`
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
	Signature       string     `json:"signatureVersion,omitempty"`
	FailureCategory string     `json:"failureCategory,omitempty"`
	ETag            string     `json:"etag,omitempty"`
	ExpectedAttempt int        `json:"-"`
}

type ScanClaimState string

const (
	ScanClaimed  ScanClaimState = "claimed"
	ScanBusy     ScanClaimState = "busy"
	ScanTerminal ScanClaimState = "terminal"
)

type ScanPoison struct {
	PoisonID        string
	EventID         string
	AssetID         string
	ETag            string
	Reason          string
	Details         string
	DequeueCount    int64
	SourceMessageID string
	BodySHA256      string
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
	FileName     string
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

const CollectionReaderRole = "media_sync_user"

type CollectionSubject struct {
	UserID string
	Roles  []string
}

type Collection struct {
	ID               string    `json:"id"`
	Namespace        string    `json:"namespace"`
	Name             string    `json:"name"`
	Revision         int64     `json:"revision"`
	RetentionDays    int       `json:"retentionDays"`
	CreatedByService string    `json:"-"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	DeletedAt        time.Time `json:"deletedAt,omitempty"`
}

type CollectionACL struct {
	ID           string      `json:"id"`
	CollectionID string      `json:"collectionId"`
	SubjectType  SubjectType `json:"subjectType"`
	SubjectID    string      `json:"subjectId"`
	Permission   Permission  `json:"permission"`
	CreatedAt    time.Time   `json:"createdAt"`
	RevokedAt    time.Time   `json:"revokedAt,omitempty"`
}

type CollectionItem struct {
	ID              string    `json:"id"`
	CollectionID    string    `json:"collectionId"`
	AssetID         string    `json:"assetId,omitempty"`
	RemoteItemID    string    `json:"remoteItemId"`
	DisplayName     string    `json:"displayName"`
	SourceRevision  string    `json:"sourceRevision"`
	CreatedRevision int64     `json:"createdRevision"`
	RetentionExempt bool      `json:"retentionExempt"`
	UpdatedRevision int64     `json:"updatedRevision"`
	DeletedRevision int64     `json:"deletedRevision,omitempty"`
	MIMEType        string    `json:"mimeType,omitempty"`
	SizeBytes       int64     `json:"sizeBytes,omitempty"`
	ETag            string    `json:"etag,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	DeletedAt       time.Time `json:"deletedAt,omitempty"`
}

type CollectionTombstone struct {
	ID              string    `json:"id"`
	RemoteItemID    string    `json:"remoteItemId"`
	DeletedRevision int64     `json:"deletedRevision"`
	DeletedAt       time.Time `json:"deletedAt"`
}

type ContentTicket struct {
	TokenHash        string
	CollectionID     string
	CollectionItemID string
	AssetETag        string
	UserID           string
	Roles            []string
	AccessMode       string
	ExpiresAt        time.Time
	CreatedAt        time.Time
}

type ContentTicketResponse struct {
	ContentURL string    `json:"contentUrl"`
	ExpiresAt  time.Time `json:"expiresAt"`
	ETag       string    `json:"etag"`
}

type ManagedContentTicket struct {
	ItemID     string    `json:"itemId"`
	ContentURL string    `json:"contentUrl"`
	ExpiresAt  time.Time `json:"expiresAt"`
	ETag       string    `json:"etag"`
}

type ManagedContentTicketBatch struct {
	Tickets            []ManagedContentTicket `json:"tickets"`
	UnavailableItemIDs []string               `json:"unavailableItemIds"`
}

type CollectionPage struct {
	Collections []Collection `json:"collections"`
	Cursor      string       `json:"cursor,omitempty"`
	HasMore     bool         `json:"hasMore"`
}

type CollectionChangePage struct {
	Collection Collection            `json:"collection"`
	Items      []CollectionItem      `json:"items"`
	Tombstones []CollectionTombstone `json:"tombstones"`
	Cursor     string                `json:"cursor"`
	HasMore    bool                  `json:"hasMore"`
	Reset      bool                  `json:"reset"`
}

type ManagedCollection struct {
	Collection Collection      `json:"collection"`
	ACLs       []CollectionACL `json:"acls"`
}

type ManagedCollectionPage struct {
	Collections []ManagedCollection `json:"collections"`
	Cursor      string              `json:"cursor,omitempty"`
	HasMore     bool                `json:"hasMore"`
}

type ManagedCollectionItem struct {
	ID              string    `json:"id"`
	DisplayName     string    `json:"displayName"`
	MIMEType        string    `json:"mimeType"`
	SizeBytes       int64     `json:"sizeBytes"`
	CreatedAt       time.Time `json:"createdAt"`
	RetentionExempt bool      `json:"retentionExempt"`
}

type ManagedCollectionItemPage struct {
	Items   []ManagedCollectionItem `json:"items"`
	Cursor  string                  `json:"cursor,omitempty"`
	HasMore bool                    `json:"hasMore"`
}

type CollectionACLMutation struct {
	Collection Collection    `json:"collection"`
	ACL        CollectionACL `json:"acl"`
}

type CollectionItemMutation struct {
	Collection Collection          `json:"collection"`
	Item       CollectionItem      `json:"item,omitempty"`
	Tombstone  CollectionTombstone `json:"tombstone,omitempty"`
}

type CreateCollectionInput struct {
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	CallerService  string `json:"-"`
	IdempotencyKey string `json:"-"`
}

type RenameCollectionInput struct {
	CollectionID   string `json:"-"`
	Name           string `json:"name"`
	CallerService  string `json:"-"`
	IdempotencyKey string `json:"-"`
}

type DeleteCollectionInput struct {
	CollectionID   string `json:"-"`
	CallerService  string `json:"-"`
	IdempotencyKey string `json:"-"`
}

type AddCollectionACLInput struct {
	CollectionID   string      `json:"-"`
	SubjectType    SubjectType `json:"subjectType"`
	SubjectID      string      `json:"subjectId"`
	Permission     Permission  `json:"permission"`
	CallerService  string      `json:"-"`
	IdempotencyKey string      `json:"-"`
}

type RevokeCollectionACLInput struct {
	CollectionID   string `json:"-"`
	ACLID          string `json:"-"`
	CallerService  string `json:"-"`
	IdempotencyKey string `json:"-"`
}

type AddCollectionItemInput struct {
	CollectionID   string `json:"-"`
	AssetID        string `json:"assetId"`
	RemoteItemID   string `json:"remoteItemId"`
	DisplayName    string `json:"displayName"`
	SourceRevision string `json:"sourceRevision"`
	CallerService  string `json:"-"`
	IdempotencyKey string `json:"-"`
}

type DeleteCollectionItemInput struct {
	CollectionID   string `json:"-"`
	ItemID         string `json:"-"`
	CallerService  string `json:"-"`
	IdempotencyKey string `json:"-"`
}

type SetCollectionItemsRetentionInput struct {
	CollectionID    string   `json:"-"`
	ItemIDs         []string `json:"itemIds"`
	RetentionExempt bool     `json:"retentionExempt"`
	CallerService   string   `json:"-"`
	IdempotencyKey  string   `json:"-"`
}

type DeleteCollectionItemsInput struct {
	CollectionID   string   `json:"-"`
	ItemIDs        []string `json:"itemIds"`
	CallerService  string   `json:"-"`
	IdempotencyKey string   `json:"-"`
}

type DeleteCollectionItemsResult struct {
	Deleted        int `json:"deleted"`
	AlreadyRemoved int `json:"alreadyRemoved"`
}

type RenameCollectionItemInput struct {
	CollectionID   string `json:"-"`
	ItemID         string `json:"-"`
	DisplayName    string `json:"displayName"`
	CallerService  string `json:"-"`
	IdempotencyKey string `json:"-"`
}

type UpdateCollectionRetentionInput struct {
	CollectionID   string `json:"-"`
	RetentionDays  int    `json:"retentionDays"`
	CallerService  string `json:"-"`
	IdempotencyKey string `json:"-"`
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
	CreateCollection(context.Context, CreateCollectionInput, time.Time) (Collection, error)
	RenameCollection(context.Context, RenameCollectionInput, time.Time) (Collection, error)
	DeleteCollection(context.Context, DeleteCollectionInput, time.Time) (Collection, error)
	AddCollectionACL(context.Context, AddCollectionACLInput, time.Time) (CollectionACLMutation, error)
	RevokeCollectionACL(context.Context, RevokeCollectionACLInput, time.Time) (CollectionACLMutation, error)
	AddCollectionItem(context.Context, AddCollectionItemInput, time.Time) (CollectionItemMutation, error)
	DeleteCollectionItem(context.Context, DeleteCollectionItemInput, time.Time) (CollectionItemMutation, error)
	SetCollectionItemsRetention(context.Context, SetCollectionItemsRetentionInput, time.Time) error
	DeleteCollectionItems(context.Context, DeleteCollectionItemsInput, time.Time) (DeleteCollectionItemsResult, error)
	RenameCollectionItem(context.Context, RenameCollectionItemInput, time.Time) (ManagedCollectionItem, error)
	ListAuthorizedCollections(context.Context, CollectionSubject, string, int) (CollectionPage, error)
	GetAuthorizedCollection(context.Context, string, CollectionSubject) (Collection, error)
	GetAuthorizedCollectionItem(context.Context, string, string, CollectionSubject) (CollectionItem, error)
	GetManagedCollectionItem(context.Context, string, string) (CollectionItem, error)
	CollectionChanges(context.Context, string, string, CollectionSubject) (CollectionChangePage, error)
	CreateContentTicket(context.Context, ContentTicket, time.Time) error
	RedeemContentTicket(context.Context, string, time.Time) (Asset, error)
	ListManagedCollections(context.Context, string, string, int) (ManagedCollectionPage, error)
	GetManagedCollection(context.Context, string, string) (ManagedCollection, error)
	ListManagedCollectionItems(context.Context, string, string, string, int) (ManagedCollectionItemPage, error)
	UpdateCollectionRetention(context.Context, UpdateCollectionRetentionInput, time.Time) (Collection, error)
}

type BlobStore interface {
	CreateUploadTarget(context.Context, string, int64, time.Time) (UploadTarget, error)
	InspectProperties(context.Context, string) (BlobMetadata, error)
	Inspect(context.Context, string, string, int64) (BlobProperties, error)
	Commit(context.Context, string, string) (BlobProperties, error)
	Open(context.Context, string, ByteRange, string) (BlobDownload, error)
	Delete(context.Context, string) error
}
