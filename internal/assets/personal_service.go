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
