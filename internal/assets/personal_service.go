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
