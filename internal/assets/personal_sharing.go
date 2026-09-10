package assets

import (
	"context"
	"fmt"
	"time"
)

type PersonalFolderGrant struct {
	ID            string     `json:"id"`
	FolderItemID  string     `json:"folderItemId"`
	OwnerUserID   string     `json:"ownerUserId"`
	GranteeUserID string     `json:"granteeUserId"`
	CreatedAt     time.Time  `json:"createdAt"`
	RevokedAt     *time.Time `json:"revokedAt,omitempty"`
}

type SharedFolderRoot struct {
	GrantID            string       `json:"grantId"`
	OwnerUserID        string       `json:"ownerUserId"`
	Root               PersonalNode `json:"root"`
	CollectionRevision int64        `json:"collectionRevision"`
}

type SharedFolderSnapshot struct {
	GrantID            string         `json:"grantId"`
	CollectionRevision int64          `json:"collectionRevision"`
	Items              []PersonalNode `json:"items"`
	Cursor             string         `json:"nextCursor"`
	HasMore            bool           `json:"hasMore"`
	Reset              bool           `json:"reset"`
}

type personalSharingRepository interface {
	CreatePersonalFolderGrant(context.Context, string, string, string, time.Time) (PersonalFolderGrant, error)
	ListPersonalFolderGrants(context.Context, string, string) ([]PersonalFolderGrant, error)
	RevokePersonalFolderGrant(context.Context, string, string, string, time.Time) error
	ListSharedFolderRoots(context.Context, string) ([]SharedFolderRoot, error)
	SharedFolderSnapshot(context.Context, string, string, string, int) (SharedFolderSnapshot, error)
	LeaveSharedFolder(context.Context, string, string, time.Time) error
	SharedFolderContentAssetID(context.Context, string, string, string, time.Time) (string, error)
}

func (s *Service) CreatePersonalFolderGrant(ctx context.Context, owner, item, grantee string) (PersonalFolderGrant, error) {
	if owner == "" {
		return PersonalFolderGrant{}, ErrUnauthorized
	}
	if item == "" || grantee == "" || owner == grantee {
		return PersonalFolderGrant{}, ErrInvalidInput
	}
	repo, ok := s.repository.(personalSharingRepository)
	if !ok {
		return PersonalFolderGrant{}, fmt.Errorf("personal sharing repository unavailable")
	}
	return repo.CreatePersonalFolderGrant(ctx, owner, item, grantee, s.now().UTC())
}

func (s *Service) ListPersonalFolderGrants(ctx context.Context, owner, item string) ([]PersonalFolderGrant, error) {
	if owner == "" {
		return nil, ErrUnauthorized
	}
	if item == "" {
		return nil, ErrInvalidInput
	}
	repo, ok := s.repository.(personalSharingRepository)
	if !ok {
		return nil, fmt.Errorf("personal sharing repository unavailable")
	}
	return repo.ListPersonalFolderGrants(ctx, owner, item)
}

func (s *Service) RevokePersonalFolderGrant(ctx context.Context, owner, item, grant string) error {
	if owner == "" {
		return ErrUnauthorized
	}
	if item == "" || grant == "" {
		return ErrInvalidInput
	}
	repo, ok := s.repository.(personalSharingRepository)
	if !ok {
		return fmt.Errorf("personal sharing repository unavailable")
	}
	return repo.RevokePersonalFolderGrant(ctx, owner, item, grant, s.now().UTC())
}

func (s *Service) ListSharedFolderRoots(ctx context.Context, recipient string) ([]SharedFolderRoot, error) {
	if recipient == "" {
		return nil, ErrUnauthorized
	}
	repo, ok := s.repository.(personalSharingRepository)
	if !ok {
		return nil, fmt.Errorf("personal sharing repository unavailable")
	}
	return repo.ListSharedFolderRoots(ctx, recipient)
}

func (s *Service) SharedFolderSnapshot(ctx context.Context, recipient, grant, cursor string, limit int) (SharedFolderSnapshot, error) {
	if recipient == "" {
		return SharedFolderSnapshot{}, ErrUnauthorized
	}
	if grant == "" {
		return SharedFolderSnapshot{}, ErrInvalidInput
	}
	repo, ok := s.repository.(personalSharingRepository)
	if !ok {
		return SharedFolderSnapshot{}, fmt.Errorf("personal sharing repository unavailable")
	}
	return repo.SharedFolderSnapshot(ctx, recipient, grant, cursor, limit)
}

func (s *Service) LeaveSharedFolder(ctx context.Context, recipient, grant string) error {
	if recipient == "" {
		return ErrUnauthorized
	}
	if grant == "" {
		return ErrInvalidInput
	}
	repo, ok := s.repository.(personalSharingRepository)
	if !ok {
		return fmt.Errorf("personal sharing repository unavailable")
	}
	return repo.LeaveSharedFolder(ctx, recipient, grant, s.now().UTC())
}

func (s *Service) SharedFolderContentMetadata(ctx context.Context, recipient, grant, item string) (PublicDownloadMetadata, error) {
	if recipient == "" {
		return PublicDownloadMetadata{}, ErrUnauthorized
	}
	if grant == "" || item == "" {
		return PublicDownloadMetadata{}, ErrInvalidInput
	}
	repo, ok := s.repository.(personalSharingRepository)
	if !ok {
		return PublicDownloadMetadata{}, fmt.Errorf("personal sharing repository unavailable")
	}
	id, err := repo.SharedFolderContentAssetID(ctx, recipient, grant, item, s.now().UTC())
	if err != nil {
		return PublicDownloadMetadata{}, err
	}
	asset, err := s.repository.GetAsset(ctx, id)
	if err != nil {
		return PublicDownloadMetadata{}, err
	}
	return collectionContentMetadata(asset, asset.ETag)
}
