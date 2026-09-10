package assets

import (
	"context"
	"fmt"
	"time"
)

type personalRepository interface {
	EnsurePersonalSpace(context.Context, string, time.Time) (PersonalSpace, error)
	PersonalChanges(context.Context, string, string, int) (PersonalChangePage, error)
	ApplyPersonalMutation(context.Context, string, PersonalMutation, time.Time) (PersonalMutationResult, error)
}

type personalGovernanceRepository interface {
	PersonalUsage(context.Context, string, time.Time) (PersonalUsage, error)
	SetPersonalQuota(context.Context, string, string, *int64, string, time.Time) (PersonalUsage, error)
	PurgePersonalTrash(context.Context, string, PersonalTrashPurgeInput, time.Time) (PersonalTrashPurgeResult, error)
}

func (s *Service) EnsurePersonalSpace(ctx context.Context, owner string) (PersonalSpace, error) {
	if owner == "" {
		return PersonalSpace{}, ErrUnauthorized
	}
	repository, ok := s.repository.(personalRepository)
	if !ok {
		return PersonalSpace{}, fmt.Errorf("personal sync repository unavailable")
	}
	return repository.EnsurePersonalSpace(ctx, owner, s.now().UTC())
}
func (s *Service) PersonalChanges(ctx context.Context, owner, cursor string, limit int) (PersonalChangePage, error) {
	if owner == "" {
		return PersonalChangePage{}, ErrUnauthorized
	}
	repository, ok := s.repository.(personalRepository)
	if !ok {
		return PersonalChangePage{}, fmt.Errorf("personal sync repository unavailable")
	}
	return repository.PersonalChanges(ctx, owner, cursor, limit)
}
func (s *Service) ApplyPersonalMutation(ctx context.Context, owner string, m PersonalMutation) (PersonalMutationResult, error) {
	if owner == "" {
		return PersonalMutationResult{}, ErrUnauthorized
	}
	if err := m.Validate(); err != nil {
		return PersonalMutationResult{}, err
	}
	repository, ok := s.repository.(personalRepository)
	if !ok {
		return PersonalMutationResult{}, fmt.Errorf("personal sync repository unavailable")
	}
	return repository.ApplyPersonalMutation(ctx, owner, m, s.now().UTC())
}

func (s *Service) PersonalUsage(ctx context.Context, owner string) (PersonalUsage, error) {
	if owner == "" {
		return PersonalUsage{}, ErrUnauthorized
	}
	repository, ok := s.repository.(personalGovernanceRepository)
	if !ok {
		return PersonalUsage{}, fmt.Errorf("personal governance repository unavailable")
	}
	return repository.PersonalUsage(ctx, owner, s.now().UTC())
}

func (s *Service) SetPersonalQuota(ctx context.Context, actor, owner string, quota *int64, requestID string) (PersonalUsage, error) {
	if actor == "" {
		return PersonalUsage{}, ErrUnauthorized
	}
	if owner == "" || len(owner) > 128 || requestID == "" || len(requestID) > 128 || (quota != nil && *quota <= 0) {
		return PersonalUsage{}, ErrInvalidInput
	}
	repository, ok := s.repository.(personalGovernanceRepository)
	if !ok {
		return PersonalUsage{}, fmt.Errorf("personal governance repository unavailable")
	}
	return repository.SetPersonalQuota(ctx, actor, owner, quota, requestID, s.now().UTC())
}

func (s *Service) PurgePersonalTrash(ctx context.Context, owner string, input PersonalTrashPurgeInput) (PersonalTrashPurgeResult, error) {
	if owner == "" {
		return PersonalTrashPurgeResult{}, ErrUnauthorized
	}
	if err := input.Validate(); err != nil {
		return PersonalTrashPurgeResult{}, err
	}
	repository, ok := s.repository.(personalGovernanceRepository)
	if !ok {
		return PersonalTrashPurgeResult{}, fmt.Errorf("personal governance repository unavailable")
	}
	return repository.PurgePersonalTrash(ctx, owner, input, s.now().UTC())
}

func (s *Service) PersonalContentMetadata(ctx context.Context, owner, item string, revision int64) (PublicDownloadMetadata, error) {
	if owner == "" {
		return PublicDownloadMetadata{}, ErrUnauthorized
	}
	if revision < 0 {
		return PublicDownloadMetadata{}, ErrInvalidInput
	}
	repository, ok := s.repository.(interface {
		PersonalContentAssetID(context.Context, string, string, int64, time.Time) (string, error)
	})
	if !ok {
		return PublicDownloadMetadata{}, fmt.Errorf("personal content repository unavailable")
	}
	id, err := repository.PersonalContentAssetID(ctx, owner, item, revision, s.now().UTC())
	if err != nil {
		return PublicDownloadMetadata{}, err
	}
	asset, err := s.repository.GetAsset(ctx, id)
	if err != nil {
		return PublicDownloadMetadata{}, err
	}
	return collectionContentMetadata(asset, asset.ETag)
}
