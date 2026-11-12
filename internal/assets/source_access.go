package assets

import (
	"context"
	"errors"
	"time"
)

// SourceAccessError separates permanent item loss from recoverable dependencies.
type SourceAccessError struct {
	Code  string
	Scope string
}

func (e *SourceAccessError) Error() string { return e.Code }

func sourceUnavailable() error {
	return &SourceAccessError{Code: "AST_SOURCE_UNAVAILABLE", Scope: "dependency"}
}

type SourceAccessRequest struct {
	Version             int           `json:"version"`
	Mode                string        `json:"mode"`
	Source              *SourcePolicy `json:"source,omitempty"`
	CollectionID        string        `json:"collectionId,omitempty"`
	ItemID              string        `json:"itemId,omitempty"`
	PublicationRevision string        `json:"publicationRevision,omitempty"`
	ACLRevision         int64         `json:"aclRevision,omitempty"`
	BindingGeneration   int64         `json:"bindingGeneration,omitempty"`
}

type SourceDecision struct {
	Version        int    `json:"version"`
	State          string `json:"state"`
	PolicyRevision int64  `json:"policyRevision"`
}

type SourceAuthority interface {
	CheckSource(context.Context, SourceAccessRequest) (SourceDecision, error)
}

type CurrentSubjectResolver interface {
	CurrentSubject(context.Context, string) (CollectionSubject, error)
}

func (s *Service) WithSourceAuthority(authority SourceAuthority, subjects CurrentSubjectResolver) *Service {
	s.sourceAuthority, s.currentSubjects = authority, subjects
	return s
}

func (s *Service) checkSource(ctx context.Context, request SourceAccessRequest) (SourceDecision, error) {
	if s.sourceAuthority == nil {
		return SourceDecision{}, sourceUnavailable()
	}
	decision, err := s.sourceAuthority.CheckSource(ctx, request)
	if err != nil || decision.Version != 1 || decision.PolicyRevision < 1 {
		return SourceDecision{}, sourceUnavailable()
	}
	scope := "item"
	if request.Source == nil {
		scope = "collection"
	}
	switch decision.State {
	case "active":
		return decision, nil
	case "paused":
		return decision, &SourceAccessError{Code: "AST_SOURCE_PAUSED", Scope: scope}
	case "revoked":
		return decision, &SourceAccessError{Code: "AST_SOURCE_REVOKED", Scope: scope}
	case "expired":
		return decision, &SourceAccessError{Code: "AST_SOURCE_EXPIRED", Scope: scope}
	default:
		return SourceDecision{}, sourceUnavailable()
	}
}

func (s *Service) LineSourceContentMetadata(ctx context.Context, id, caller string) (PublicDownloadMetadata, error) {
	if caller != LineHarnessCaller {
		return PublicDownloadMetadata{}, ErrForbidden
	}
	asset, err := s.repository.GetAsset(ctx, id)
	if err != nil {
		return PublicDownloadMetadata{}, err
	}
	return s.sourceContentMetadata(ctx, asset, SourceAccessRequest{Version: 1, Mode: "original", Source: asset.Source})
}

func (s *Service) sourceContentMetadata(ctx context.Context, asset Asset, request SourceAccessRequest) (PublicDownloadMetadata, error) {
	if !asset.IsLineSource() {
		return PublicDownloadMetadata{}, ErrForbidden
	}
	if !s.now().Before(asset.Source.ExpiresAt) {
		return PublicDownloadMetadata{}, &SourceAccessError{Code: "AST_SOURCE_EXPIRED", Scope: "item"}
	}
	if !asset.DeletedAt.IsZero() || !asset.PurgedAt.IsZero() {
		return PublicDownloadMetadata{}, &SourceAccessError{Code: "AST_SOURCE_REVOKED", Scope: "item"}
	}
	if !asset.SourceAvailable(s.now()) || asset.UploadStatus != UploadCompleted || asset.ScanStatus != ScanNotRequired || asset.ProcessingStatus != ProcessingNotRequired || asset.ETag == "" {
		return PublicDownloadMetadata{}, ErrNotFound
	}
	if _, err := s.checkSource(ctx, request); err != nil {
		return PublicDownloadMetadata{}, err
	}
	if !s.now().Before(asset.Source.ExpiresAt) {
		return PublicDownloadMetadata{}, &SourceAccessError{Code: "AST_SOURCE_EXPIRED", Scope: "item"}
	}
	return PublicDownloadMetadata{SourceExpiresAt: asset.Source.ExpiresAt, Size: asset.SizeBytes, ContentType: asset.DetectedMIMEType, FileName: asset.OriginalFileName, ETag: asset.ETag, LastModified: asset.UpdatedAt, CacheControl: "private, no-store", objectKey: asset.ObjectKey}, nil
}

type CollectionSourcePolicy struct {
	Version           int    `json:"version"`
	CollectionID      string `json:"collectionId"`
	ACLRevision       int64  `json:"aclRevision"`
	BindingGeneration int64  `json:"bindingGeneration"`
}

type StageSourceItemInput struct {
	AddCollectionItemInput
	BindingGeneration   int64 `json:"bindingGeneration"`
	ExpectedACLRevision int64 `json:"expectedAclRevision"`
}

type ActivateSourceItemInput struct {
	CollectionID        string `json:"-"`
	ItemID              string `json:"-"`
	CallerService       string `json:"-"`
	IdempotencyKey      string `json:"-"`
	BindingGeneration   int64  `json:"bindingGeneration"`
	ExpectedACLRevision int64  `json:"expectedAclRevision"`
}

type SourcePublicationRepository interface {
	BindSourceCollection(context.Context, BindSourceCollectionInput, time.Time) (Collection, error)
	GetSourcePublicationItem(context.Context, string, string, string) (CollectionItemMutation, error)
	SourceContentTicket(context.Context, string, time.Time) (ContentTicket, bool, error)
	GetCollectionSourcePolicy(context.Context, string) (CollectionSourcePolicy, error)
	StageSourceItem(context.Context, StageSourceItemInput, time.Time) (CollectionItemMutation, error)
	ActivateSourceItem(context.Context, ActivateSourceItemInput, time.Time) (CollectionItemMutation, error)
	GetSourcePublication(context.Context, string, string, string) (CollectionItemMutation, error)
}

func sourceACLRevoked() error {
	return &SourceAccessError{Code: "AST_SOURCE_ACL_REVOKED", Scope: "collection"}
}

func (s *Service) currentSourceSubject(ctx context.Context, id string, subject CollectionSubject) (CollectionSubject, CollectionSourcePolicy, error) {
	repo, ok := s.repository.(SourcePublicationRepository)
	if !ok {
		return subject, CollectionSourcePolicy{}, nil
	}
	p, err := repo.GetCollectionSourcePolicy(ctx, id)
	if err != nil {
		return CollectionSubject{}, p, err
	}
	if p.Version == 0 {
		return subject, p, nil
	}
	if p.Version != 1 || p.ACLRevision < 1 || p.BindingGeneration < 1 || s.currentSubjects == nil {
		return CollectionSubject{}, p, sourceUnavailable()
	}
	current, err := s.currentSubjects.CurrentSubject(ctx, subject.UserID)
	if errors.Is(err, ErrForbidden) || errors.Is(err, ErrUnauthorized) {
		return CollectionSubject{}, p, sourceACLRevoked()
	}
	if err != nil || current.UserID != subject.UserID || !validCollectionSubject(current) {
		return CollectionSubject{}, p, sourceUnavailable()
	}
	return current, p, nil
}

func sourceRequest(p CollectionSourcePolicy, mode string, a Asset, item CollectionItem) SourceAccessRequest {
	return SourceAccessRequest{Version: 1, Mode: mode, CollectionID: p.CollectionID, ACLRevision: p.ACLRevision, BindingGeneration: p.BindingGeneration, Source: a.Source, ItemID: item.ID, PublicationRevision: item.SourceRevision}
}

func (s *Service) sourceAuthorizedItem(ctx context.Context, collectionID, itemID string, subject CollectionSubject) (CollectionItem, Asset, error) {
	current, p, err := s.currentSourceSubject(ctx, collectionID, subject)
	if err != nil {
		return CollectionItem{}, Asset{}, err
	}
	if p.Version == 0 {
		item, err := s.repository.GetAuthorizedCollectionItem(ctx, collectionID, itemID, current)
		return item, Asset{}, err
	}
	if _, err := s.repository.GetAuthorizedCollection(ctx, collectionID, current); err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
			err = sourceACLRevoked()
		}
		return CollectionItem{}, Asset{}, err
	}
	item, err := s.repository.GetAuthorizedCollectionItem(ctx, collectionID, itemID, current)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			err = &SourceAccessError{Code: "AST_SOURCE_REVOKED", Scope: "item"}
		}
		return CollectionItem{}, Asset{}, err
	}
	a, err := s.repository.GetAsset(ctx, item.AssetID)
	if err != nil {
		return CollectionItem{}, Asset{}, err
	}
	if !a.IsLineSource() || item.ETag != a.ETag || item.PublicationState != "active" {
		return CollectionItem{}, Asset{}, sourceUnavailable()
	}
	if _, err := s.sourceContentMetadata(ctx, a, sourceRequest(p, "published", a, item)); err != nil {
		return CollectionItem{}, Asset{}, err
	}
	latest, err := s.repository.(SourcePublicationRepository).GetCollectionSourcePolicy(ctx, collectionID)
	if err != nil || latest != p {
		return CollectionItem{}, Asset{}, sourceUnavailable()
	}
	item.SourceExpiresAt = a.Source.ExpiresAt
	return item, a, nil
}

type CollectionControl struct {
	Version           int    `json:"version"`
	CollectionID      string `json:"collectionId"`
	State             string `json:"state"`
	PolicyRevision    int64  `json:"policyRevision"`
	ACLRevision       int64  `json:"aclRevision"`
	BindingGeneration int64  `json:"bindingGeneration"`
}

func (s *Service) CollectionControl(ctx context.Context, id string, subject CollectionSubject) (CollectionControl, error) {
	if id == "" || !validCollectionSubject(subject) {
		return CollectionControl{}, ErrForbidden
	}
	current, p, err := s.currentSourceSubject(ctx, id, subject)
	if err != nil {
		return CollectionControl{}, err
	}
	if p.Version != 1 {
		return CollectionControl{}, ErrNotFound
	}
	if _, err := s.repository.GetAuthorizedCollection(ctx, id, current); err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
			err = sourceACLRevoked()
		}
		return CollectionControl{}, err
	}
	decision, err := s.checkSource(ctx, sourceRequest(p, "control", Asset{}, CollectionItem{}))
	if err != nil {
		var access *SourceAccessError
		if !errors.As(err, &access) || access.Scope == "dependency" {
			return CollectionControl{}, err
		}
	}
	return CollectionControl{Version: 1, CollectionID: id, State: decision.State, PolicyRevision: decision.PolicyRevision, ACLRevision: p.ACLRevision, BindingGeneration: p.BindingGeneration}, nil
}

func (s *Service) sourceChanges(ctx context.Context, id, cursor string, subject CollectionSubject) (CollectionChangePage, error) {
	current, p, err := s.currentSourceSubject(ctx, id, subject)
	if err != nil {
		return CollectionChangePage{}, err
	}
	if p.Version == 0 {
		return s.repository.CollectionChanges(ctx, id, cursor, current)
	}
	if _, err := s.repository.GetAuthorizedCollection(ctx, id, current); err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
			err = sourceACLRevoked()
		}
		return CollectionChangePage{}, err
	}
	if _, err := s.checkSource(ctx, sourceRequest(p, "collection", Asset{}, CollectionItem{})); err != nil {
		return CollectionChangePage{}, err
	}
	page, err := s.repository.CollectionChanges(ctx, id, cursor, current)
	if err != nil {
		return CollectionChangePage{}, err
	}
	items := make([]CollectionItem, 0, len(page.Items))
	for _, item := range page.Items {
		a, err := s.repository.GetAsset(ctx, item.AssetID)
		if err == nil {
			if !a.IsLineSource() {
				return CollectionChangePage{}, sourceUnavailable()
			}
			_, err = s.sourceContentMetadata(ctx, a, sourceRequest(p, "published", a, item))
		}
		if err != nil {
			var access *SourceAccessError
			if errors.Is(err, ErrNotFound) || (errors.As(err, &access) && (access.Code == "AST_SOURCE_EXPIRED" || access.Code == "AST_SOURCE_REVOKED")) {
				page.Tombstones = append(page.Tombstones, CollectionTombstone{ID: item.ID, RemoteItemID: item.RemoteItemID, DeletedRevision: page.Collection.Revision, DeletedAt: s.now().UTC()})
				continue
			}
			return CollectionChangePage{}, err
		}
		item.SourceExpiresAt = a.Source.ExpiresAt
		items = append(items, item)
	}
	latest, err := s.repository.(SourcePublicationRepository).GetCollectionSourcePolicy(ctx, id)
	if err != nil || latest != p {
		return CollectionChangePage{}, sourceUnavailable()
	}
	page.Items = items
	return page, nil
}

func (s *Service) StageSourceItem(ctx context.Context, input StageSourceItemInput) (CollectionItemMutation, error) {
	if input.CallerService != LineHarnessCaller {
		return CollectionItemMutation{}, ErrForbidden
	}
	if input.CollectionID == "" || input.AssetID == "" || !opaqueSourceID.MatchString(input.RemoteItemID) || !opaqueSourceID.MatchString(input.SourceRevision) || !validCollectionItemDisplayName(input.DisplayName) || !validMutationIdentity(input.CallerService, input.IdempotencyKey) || input.BindingGeneration < 1 || input.ExpectedACLRevision < 1 {
		return CollectionItemMutation{}, ErrInvalidInput
	}
	repo, ok := s.repository.(SourcePublicationRepository)
	if !ok {
		return CollectionItemMutation{}, sourceUnavailable()
	}
	p, err := repo.GetCollectionSourcePolicy(ctx, input.CollectionID)
	if err != nil {
		return CollectionItemMutation{}, err
	}
	if p.Version != 1 || p.ACLRevision != input.ExpectedACLRevision || p.BindingGeneration != input.BindingGeneration {
		return CollectionItemMutation{}, ErrConflict
	}
	a, err := s.repository.GetAsset(ctx, input.AssetID)
	if err != nil {
		return CollectionItemMutation{}, err
	}
	if _, err := s.sourceContentMetadata(ctx, a, sourceRequest(p, "stage", a, CollectionItem{SourceRevision: input.SourceRevision})); err != nil {
		return CollectionItemMutation{}, err
	}
	return repo.StageSourceItem(ctx, input, s.now().UTC())
}

func (s *Service) GetSourcePublication(ctx context.Context, collectionID, operation, caller string) (CollectionItemMutation, error) {
	if caller != LineHarnessCaller {
		return CollectionItemMutation{}, ErrForbidden
	}
	if collectionID == "" || operation == "" || len(operation) > 128 {
		return CollectionItemMutation{}, ErrInvalidInput
	}
	repo, ok := s.repository.(SourcePublicationRepository)
	if !ok {
		return CollectionItemMutation{}, sourceUnavailable()
	}
	return repo.GetSourcePublication(ctx, collectionID, operation, caller)
}

func (s *Service) ActivateSourceItem(ctx context.Context, input ActivateSourceItemInput) (CollectionItemMutation, error) {
	if input.CallerService != LineHarnessCaller {
		return CollectionItemMutation{}, ErrForbidden
	}
	if input.CollectionID == "" || input.ItemID == "" || !validMutationIdentity(input.CallerService, input.IdempotencyKey) || input.BindingGeneration < 1 || input.ExpectedACLRevision < 1 {
		return CollectionItemMutation{}, ErrInvalidInput
	}
	repo, ok := s.repository.(SourcePublicationRepository)
	if !ok {
		return CollectionItemMutation{}, sourceUnavailable()
	}
	v, err := repo.GetSourcePublicationItem(ctx, input.CollectionID, input.ItemID, input.CallerService)
	if err != nil {
		return CollectionItemMutation{}, err
	}
	c := v.Collection
	if !c.DeletedAt.IsZero() || c.SourcePolicyVersion != 1 || c.ACLRevision != input.ExpectedACLRevision || c.BindingGeneration != input.BindingGeneration || v.Item.DeletedRevision != 0 {
		return CollectionItemMutation{}, ErrConflict
	}
	a, err := s.repository.GetAsset(ctx, v.Item.AssetID)
	if err != nil {
		return CollectionItemMutation{}, err
	}
	p := CollectionSourcePolicy{Version: 1, CollectionID: c.ID, ACLRevision: c.ACLRevision, BindingGeneration: c.BindingGeneration}
	if _, err := s.sourceContentMetadata(ctx, a, sourceRequest(p, "activate", a, v.Item)); err != nil {
		return CollectionItemMutation{}, err
	}
	return repo.ActivateSourceItem(ctx, input, s.now().UTC())
}

type BindSourceCollectionInput struct {
	CollectionID              string `json:"-"`
	CallerService             string `json:"-"`
	IdempotencyKey            string `json:"-"`
	ExpectedBindingGeneration int64  `json:"expectedBindingGeneration"`
	BindingGeneration         int64  `json:"bindingGeneration"`
	ExpectedACLRevision       int64  `json:"expectedAclRevision"`
}

func (s *Service) BindSourceCollection(ctx context.Context, input BindSourceCollectionInput) (Collection, error) {
	if input.CallerService != LineHarnessCaller {
		return Collection{}, ErrForbidden
	}
	if input.CollectionID == "" || input.ExpectedBindingGeneration < 1 || input.BindingGeneration <= input.ExpectedBindingGeneration || input.BindingGeneration > 1<<53-1 || input.ExpectedACLRevision < 1 || !validMutationIdentity(input.CallerService, input.IdempotencyKey) {
		return Collection{}, ErrInvalidInput
	}
	repo, ok := s.repository.(SourcePublicationRepository)
	if !ok {
		return Collection{}, sourceUnavailable()
	}
	return repo.BindSourceCollection(ctx, input, s.now().UTC())
}

func (s *Service) CheckContentDeadline(metadata PublicDownloadMetadata) error {
	if !metadata.SourceExpiresAt.IsZero() && !s.now().Before(metadata.SourceExpiresAt) {
		return &SourceAccessError{Code: "AST_SOURCE_EXPIRED", Scope: "item"}
	}
	return nil
}

// GetSourceUpload recovers an uncertain create without issuing another upload target.
func (s *Service) GetSourceUpload(ctx context.Context, operation, caller string) (CreatedUpload, error) {
	if caller != LineHarnessCaller {
		return CreatedUpload{}, ErrForbidden
	}
	if !opaqueSourceID.MatchString(operation) {
		return CreatedUpload{}, ErrInvalidInput
	}
	repo, ok := s.repository.(interface {
		FindUploadByIdempotency(context.Context, string, string, string) (Asset, UploadSession, error)
	})
	if !ok {
		return CreatedUpload{}, sourceUnavailable()
	}
	asset, session, err := repo.FindUploadByIdempotency(ctx, caller, "create_upload", operation)
	if err != nil {
		return CreatedUpload{}, err
	}
	if !asset.IsLineSource() || asset.OwnerService != caller || session.CallerService != caller {
		return CreatedUpload{}, ErrForbidden
	}
	return CreatedUpload{Asset: asset, Session: session}, nil
}
