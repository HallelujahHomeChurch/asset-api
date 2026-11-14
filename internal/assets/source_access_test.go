package assets

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type sourceGateFunc func(context.Context, SourceAccessRequest) (SourceDecision, error)

func (f sourceGateFunc) CheckSource(ctx context.Context, r SourceAccessRequest) (SourceDecision, error) {
	return f(ctx, r)
}

func TestLineOriginalRequiresCurrentSourceAuthority(t *testing.T) {
	now := time.Now().UTC()
	a := Asset{ID: "original", Namespace: "line.group.file", OwnerService: LineHarnessCaller, UploadProfiles: UploadProfiles{LineSourceSecurityProfile, noProcessing}, Source: &SourcePolicy{SourceReference: SourceReference{Ref: "source_opaque_123", Revision: "revision_opaque_123", PublishedAt: now.Add(-time.Hour)}, Version: 1, ExpiresAt: now.Add(-time.Hour).Add(SourceLifetime)}, UploadStatus: UploadCompleted, ScanStatus: ScanNotRequired, ProcessingStatus: ProcessingNotRequired, ETag: "immutable", ObjectKey: "private/original", SizeBytes: 20, DetectedMIMEType: "application/pdf"}
	for _, tc := range []struct {
		name, state, want string
		version           int
		fail              bool
	}{
		{"active", "active", "", 1, false}, {"paused", "paused", "AST_SOURCE_PAUSED", 1, false}, {"revoked", "revoked", "AST_SOURCE_REVOKED", 1, false}, {"expired", "expired", "AST_SOURCE_EXPIRED", 1, false}, {"unknown state", "future", "AST_SOURCE_UNAVAILABLE", 1, false}, {"unknown version", "active", "AST_SOURCE_UNAVAILABLE", 2, false}, {"failure", "active", "AST_SOURCE_UNAVAILABLE", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			svc := NewService(&metadataReadRepository{asset: a}, nil, "", func() time.Time { return now }).WithSourceAuthority(sourceGateFunc(func(_ context.Context, r SourceAccessRequest) (SourceDecision, error) {
				calls++
				if r.Version != 1 || r.Mode != "original" || r.Source == nil || !r.Source.Matches(*a.Source) || r.CollectionID != "" {
					t.Fatalf("authority context %+v", r)
				}
				if tc.fail {
					return SourceDecision{}, errors.New("private provider error")
				}
				return SourceDecision{Version: tc.version, RevocationRevision: 1, State: tc.state, PolicyRevision: 3}, nil
			}), nil)
			metadata, err := svc.LineSourceContentMetadata(context.Background(), a.ID, LineHarnessCaller)
			if tc.want == "" {
				if err != nil || metadata.ETag != a.ETag || metadata.CacheControl != "private, no-store" {
					t.Fatalf("metadata %+v %v", metadata, err)
				}
			} else {
				var access *SourceAccessError
				if !errors.As(err, &access) || access.Code != tc.want || access.Scope != func() string {
					if tc.want == "AST_SOURCE_UNAVAILABLE" {
						return "dependency"
					}
					return "item"
				}() {
					t.Fatalf("error %v", err)
				}
			}
			if calls != 1 {
				t.Fatalf("authority calls %d", calls)
			}
		})
	}
	svc := NewService(&metadataReadRepository{asset: a}, nil, "", func() time.Time { return now })
	if _, err := svc.LineSourceContentMetadata(context.Background(), a.ID, LineHarnessCaller); err == nil {
		t.Fatal("missing authority admitted")
	}
	if _, err := svc.LineSourceContentMetadata(context.Background(), a.ID, "hhc-line-function-bot"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("legacy admitted %v", err)
	}
	now = a.Source.ExpiresAt
	_, err := svc.LineSourceContentMetadata(context.Background(), a.ID, LineHarnessCaller)
	var access *SourceAccessError
	if !errors.As(err, &access) || access.Code != "AST_SOURCE_EXPIRED" {
		t.Fatalf("hard deadline %v", err)
	}
}

type subjectResolverFunc func(context.Context, string) (CollectionSubject, error)

func (f subjectResolverFunc) CurrentSubject(c context.Context, id string) (CollectionSubject, error) {
	return f(c, id)
}

type sourceReaderRepository struct {
	Repository
	SourcePublicationRepository
	asset      Asset
	item       CollectionItem
	policy     CollectionSourcePolicy
	collection Collection
}

func (r *sourceReaderRepository) GetCollectionSourcePolicy(context.Context, string) (CollectionSourcePolicy, error) {
	return r.policy, nil
}
func (r *sourceReaderRepository) GetAsset(context.Context, string) (Asset, error) {
	return r.asset, nil
}
func (r *sourceReaderRepository) GetAuthorizedCollection(_ context.Context, _ string, subject CollectionSubject) (Collection, error) {
	for _, role := range subject.RoleIDs {
		if role == "allowed" {
			return r.collection, nil
		}
	}
	return Collection{}, ErrNotFound
}
func (r *sourceReaderRepository) GetAuthorizedCollectionItem(ctx context.Context, _ string, _ string, subject CollectionSubject) (CollectionItem, error) {
	if _, err := r.GetAuthorizedCollection(ctx, "", subject); err != nil {
		return CollectionItem{}, err
	}
	return r.item, nil
}

func TestSourceReaderRefreshesRolesAndUsesStoredPublicationContext(t *testing.T) {
	now := time.Now().UTC()
	repo := &sourceReaderRepository{policy: CollectionSourcePolicy{Version: 1, CollectionID: "collection", ACLRevision: 7, BindingGeneration: 3}, collection: Collection{ID: "collection", SourcePolicyVersion: 1, ACLRevision: 7, BindingGeneration: 3, CreatedByService: LineHarnessCaller}, item: CollectionItem{ID: "item", CollectionID: "collection", AssetID: "original", SourceRevision: "publication_revision_123", PublicationState: "active", ETag: "immutable"}}
	repo.asset = Asset{ID: "original", Namespace: "line.group.file", OwnerService: LineHarnessCaller, UploadProfiles: UploadProfiles{LineSourceSecurityProfile, noProcessing}, Source: &SourcePolicy{SourceReference: SourceReference{Ref: "source_opaque_123", Revision: "revision_opaque_123", PublishedAt: now}, Version: 1, ExpiresAt: now.Add(SourceLifetime)}, UploadStatus: UploadCompleted, ScanStatus: ScanNotRequired, ProcessingStatus: ProcessingNotRequired, ETag: "immutable"}
	subjectCalls, sourceCalls := 0, 0
	authority := sourceGateFunc(func(_ context.Context, r SourceAccessRequest) (SourceDecision, error) {
		sourceCalls++
		if r.CollectionID != "collection" || r.ItemID != "item" || r.PublicationRevision != repo.item.SourceRevision || r.ACLRevision != 7 || r.BindingGeneration != 3 || r.Source == nil || r.Source.Ref != repo.asset.Source.Ref {
			t.Fatalf("wrong persisted context %+v", r)
		}
		return SourceDecision{Version: 1, RevocationRevision: 1, State: "active", PolicyRevision: 9}, nil
	})
	svc := NewService(repo, nil, "", func() time.Time { return now }).WithSourceAuthority(authority, subjectResolverFunc(func(_ context.Context, id string) (CollectionSubject, error) {
		subjectCalls++
		return CollectionSubject{UserID: id, RoleIDs: []string{"allowed"}}, nil
	}))
	item, err := svc.GetAuthorizedCollectionItem(context.Background(), "collection", "item", CollectionSubject{UserID: "reader", SourcePolicyVersion: 1, RoleIDs: []string{"stale"}})
	if err != nil || item.ID != "item" || subjectCalls != 1 || sourceCalls != 1 || !item.SourceExpiresAt.Equal(repo.asset.Source.ExpiresAt) {
		t.Fatalf("reader %+v %v subject=%d source=%d", item, err, subjectCalls, sourceCalls)
	}
	svc.WithSourceAuthority(authority, subjectResolverFunc(func(_ context.Context, id string) (CollectionSubject, error) {
		return CollectionSubject{UserID: id, RoleIDs: []string{}}, nil
	}))
	_, err = svc.GetAuthorizedCollectionItem(context.Background(), "collection", "item", CollectionSubject{UserID: "reader", SourcePolicyVersion: 1, RoleIDs: []string{"allowed"}})
	var access *SourceAccessError
	if !errors.As(err, &access) || access.Code != "AST_SOURCE_ACL_REVOKED" || sourceCalls != 1 {
		t.Fatalf("stale roles granted: %v", err)
	}
	svc.WithSourceAuthority(authority, nil)
	if _, err := svc.GetAuthorizedCollectionItem(context.Background(), "collection", "item", CollectionSubject{UserID: "reader", SourcePolicyVersion: 1, RoleIDs: []string{"allowed"}}); err == nil {
		t.Fatal("missing current subject allowed")
	}
}

func TestSourceDeadlineIsRecheckedBeforeOpeningBytes(t *testing.T) {
	now := time.Now().UTC()
	service := NewService(nil, nil, "", func() time.Time { return now })
	_, err := service.OpenPublicMetadata(context.Background(), PublicDownloadMetadata{SourceExpiresAt: now}, ByteRange{})
	var access *SourceAccessError
	if !errors.As(err, &access) || access.Code != "AST_SOURCE_EXPIRED" {
		t.Fatalf("expired open %v", err)
	}
}

func TestLegacyCollectionListIgnoresSourceDependencies(t *testing.T) {
	repo := &collectionServiceRepository{}
	svc := NewService(repo, nil, "", time.Now).WithSourceAuthority(sourceGateFunc(func(context.Context, SourceAccessRequest) (SourceDecision, error) {
		t.Fatal("legacy list called source authority")
		return SourceDecision{}, nil
	}), subjectResolverFunc(func(context.Context, string) (CollectionSubject, error) {
		return CollectionSubject{}, errors.New("Account unavailable")
	}))
	_, err := svc.ListAuthorizedCollections(context.Background(), CollectionSubject{UserID: "legacy-reader"}, "", 10)
	if err != nil || repo.readerCalls != 1 {
		t.Fatalf("legacy list lost local ACL path: err=%v repositoryCalls=%d", err, repo.readerCalls)
	}
}

func (f sourceGateFunc) CheckSources(ctx context.Context, requests []SourceAccessRequest) ([]SourceDecision, error) {
	result := make([]SourceDecision, 0, len(requests))
	for _, r := range requests {
		d, err := f.CheckSource(ctx, r)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, nil
}

type batchReaderRepository struct {
	sourceReaderRepository
	page      CollectionChangePage
	bulkCalls int
}

func (r *batchReaderRepository) CollectionChanges(context.Context, string, string, CollectionSubject) (CollectionChangePage, error) {
	return r.page, nil
}
func (r *batchReaderRepository) GetAsset(context.Context, string) (Asset, error) {
	panic("per-item metadata query")
}
func (r *batchReaderRepository) GetSourceAssets(_ context.Context, ids []string) (map[string]Asset, error) {
	r.bulkCalls++
	m := map[string]Asset{}
	for _, id := range ids {
		a := r.asset
		a.ID = id
		m[id] = a
	}
	return m, nil
}

type batchReaderAuthority struct {
	batches, singles int
	fault            string
	after            func()
}

func (a *batchReaderAuthority) CheckSource(context.Context, SourceAccessRequest) (SourceDecision, error) {
	a.singles++
	if a.fault == "unsend" && a.singles == 2 {
		return SourceDecision{Version: 1, RevocationRevision: 2, State: "active", PolicyRevision: 9}, nil
	}
	return SourceDecision{Version: 1, RevocationRevision: 1, State: "active", PolicyRevision: 9}, nil
}
func (a *batchReaderAuthority) CheckSources(_ context.Context, requests []SourceAccessRequest) ([]SourceDecision, error) {
	a.batches++
	if len(requests) > 100 {
		panic("unbounded batch")
	}
	if a.fault == "unavailable" && a.batches == 2 {
		return nil, errors.New("unavailable")
	}
	result := make([]SourceDecision, len(requests))
	for i := range result {
		result[i] = SourceDecision{Version: 1, RevocationRevision: 1, State: "active", PolicyRevision: 9}
	}
	if a.batches == 1 {
		switch a.fault {
		case "missing-source":
			result[0] = SourceDecision{Version: 1, RevocationRevision: 1, State: "revoked", PolicyRevision: 1}
		case "revoked", "paused":
			result[0].State = a.fault
		case "revision":
			result[0].PolicyRevision++
		case "missing":
			result = result[:len(result)-1]
		}
	}
	if a.after != nil {
		a.after()
	}
	return result, nil
}
func TestSourceChangePageBatchesWithoutPartialCursor(t *testing.T) {
	for _, fault := range []string{"", "revoked", "paused", "unavailable", "revision", "missing", "deadline", "acl", "unsend", "missing-source"} {
		t.Run(fault, func(t *testing.T) {
			now := time.Now().UTC()
			repo := &batchReaderRepository{sourceReaderRepository: sourceReaderRepository{policy: CollectionSourcePolicy{Version: 1, CollectionID: "collection", ACLRevision: 7, BindingGeneration: 3}, collection: Collection{ID: "collection", SourcePolicyVersion: 1}}}
			repo.asset = Asset{Namespace: "line.group.file", OwnerService: LineHarnessCaller, UploadProfiles: UploadProfiles{LineSourceSecurityProfile, noProcessing}, Source: &SourcePolicy{SourceReference: SourceReference{Ref: "source_opaque_123", Revision: "revision_opaque_123", PublishedAt: now}, Version: 1, ExpiresAt: now.Add(SourceLifetime)}, UploadStatus: UploadCompleted, ScanStatus: ScanNotRequired, ProcessingStatus: ProcessingNotRequired, ETag: "immutable"}
			repo.page = CollectionChangePage{Collection: repo.collection, Cursor: "next", HasMore: true}
			for i := 0; i < 500; i++ {
				repo.page.Items = append(repo.page.Items, CollectionItem{ID: fmt.Sprint(i), AssetID: fmt.Sprint(i), ETag: "immutable", PublicationState: "active"})
			}
			gate := &batchReaderAuthority{fault: fault}
			if fault == "deadline" {
				gate.after = func() { now = repo.asset.Source.ExpiresAt }
			}
			if fault == "acl" {
				gate.after = func() { repo.policy.ACLRevision++ }
			}
			svc := NewService(repo, nil, "", func() time.Time { return now }).WithSourceAuthority(gate, subjectResolverFunc(func(context.Context, string) (CollectionSubject, error) {
				return CollectionSubject{UserID: "reader", RoleIDs: []string{"allowed"}}, nil
			}))
			page, err := svc.sourceChanges(context.Background(), "collection", "previous", CollectionSubject{UserID: "reader", SourcePolicyVersion: 1})
			if fault == "" || fault == "revoked" || fault == "missing-source" || fault == "deadline" {
				expected := 500
				if fault == "revoked" || fault == "missing-source" {
					expected = 499
				}
				if fault == "deadline" {
					expected = 0
				}
				if err != nil || len(page.Items) != expected || len(page.Tombstones) != 500-expected || page.Cursor != "next" {
					t.Fatalf("page items=%d tombstones=%d cursor=%q err=%v", len(page.Items), len(page.Tombstones), page.Cursor, err)
				}
				if gate.batches != 5 || gate.singles != 2 {
					t.Fatalf("calls batch=%d single=%d", gate.batches, gate.singles)
				}
			} else if err == nil || page.Cursor != "" || len(page.Items) > 0 {
				t.Fatalf("failure advanced %+v %v", page, err)
			}
			if repo.bulkCalls != 1 {
				t.Fatalf("metadata queries=%d", repo.bulkCalls)
			}
		})
	}
}
