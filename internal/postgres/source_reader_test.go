package postgres

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

type readerSourceAuthority struct {
	state    string
	fail     bool
	requests []assets.SourceAccessRequest
}

func (a *readerSourceAuthority) CheckSource(_ context.Context, r assets.SourceAccessRequest) (assets.SourceDecision, error) {
	a.requests = append(a.requests, r)
	if a.fail {
		return assets.SourceDecision{}, errors.New("synthetic unavailable")
	}
	return assets.SourceDecision{Version: 1, State: a.state, PolicyRevision: 4}, nil
}

type readerCurrentSubjects struct {
	roles []string
	fail  bool
}

func (s *readerCurrentSubjects) CurrentSubject(_ context.Context, id string) (assets.CollectionSubject, error) {
	if s.fail {
		return assets.CollectionSubject{}, errors.New("synthetic Account outage")
	}
	return assets.CollectionSubject{UserID: id, RoleIDs: s.roles}, nil
}

func sourceReaderFixture(t *testing.T) (*Store, assets.Collection, assets.CollectionItem, assets.Asset, time.Time) {
	t.Helper()
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	a := insertLineSource(t, db, "reader-publication", now)
	c, err := store.CreateCollection(ctx, assets.CreateCollectionInput{Namespace: "line.group.media-sync", Name: "group", CallerService: assets.LineHarnessCaller, IdempotencyKey: "collection", BindingGeneration: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AddCollectionACL(ctx, assets.AddCollectionACLInput{CollectionID: c.ID, SubjectType: assets.SubjectRole, SubjectID: "allowed", Permission: assets.PermissionRead, CallerService: assets.LineHarnessCaller, ActorUserID: "admin", RequestID: "request", IdempotencyKey: "acl", ExpectedACLRevision: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := store.StageSourceItem(ctx, assets.StageSourceItemInput{AddCollectionItemInput: assets.AddCollectionItemInput{CollectionID: c.ID, AssetID: a.ID, RemoteItemID: "opaque-item", DisplayName: "file.pdf", SourceRevision: "publication_revision_123", CallerService: assets.LineHarnessCaller, IdempotencyKey: "stage"}, BindingGeneration: 1, ExpectedACLRevision: 2}, now)
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.ActivateSourceItem(ctx, assets.ActivateSourceItemInput{CollectionID: c.ID, ItemID: stage.Item.ID, CallerService: assets.LineHarnessCaller, IdempotencyKey: "activate", BindingGeneration: 1, ExpectedACLRevision: 2}, now)
	if err != nil {
		t.Fatal(err)
	}
	return store, active.Collection, active.Item, a, now
}

func TestSourceReaderTicketsRecheckIdentityRolesAndAuthority(t *testing.T) {
	store, c, item, a, now := sourceReaderFixture(t)
	ctx := context.Background()
	authority := &readerSourceAuthority{state: "active"}
	subjects := &readerCurrentSubjects{roles: []string{"allowed"}}
	svc := assets.NewService(store, nil, "", func() time.Time { return now }).WithSourceAuthority(authority, subjects)
	subject := assets.CollectionSubject{UserID: "reader", RoleIDs: []string{"stale"}, SourcePolicyVersion: 1}
	metadata, err := svc.AuthorizedCollectionContentMetadata(ctx, c.ID, item.ID, subject)
	if err != nil || metadata.ETag != a.ETag {
		t.Fatalf("content %+v %v", metadata, err)
	}
	ticket, err := svc.IssueCollectionContentTicket(ctx, c.ID, item.ID, subject, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	location, _ := url.Parse(ticket.ContentURL)
	token := location.Query().Get("ticket")
	if _, err := svc.ContentTicketMetadata(ctx, token); !errors.Is(err, assets.ErrUnauthorized) {
		t.Fatalf("anonymous ticket: %v", err)
	}
	if _, err := svc.ContentTicketMetadataForSubject(ctx, token, assets.CollectionSubject{UserID: "other"}); !errors.Is(err, assets.ErrUnauthorized) {
		t.Fatalf("forwarded ticket: %v", err)
	}
	if _, err := svc.ContentTicketMetadataForSubject(ctx, token, subject); err != nil {
		t.Fatal(err)
	}
	subjects.roles = nil
	if _, err := svc.ContentTicketMetadataForSubject(ctx, token, subject); sourceCode(err) != "AST_SOURCE_ACL_REVOKED" {
		t.Fatalf("stale ticket roles: %v", err)
	}
	subjects.roles = []string{"allowed"}
	authority.state = "paused"
	if _, err := svc.ContentTicketMetadataForSubject(ctx, token, subject); sourceCode(err) != "AST_SOURCE_PAUSED" {
		t.Fatalf("paused ticket: %v", err)
	}
	control, err := svc.CollectionControl(ctx, c.ID, subject)
	if err != nil || control.State != "paused" || control.ACLRevision != 2 {
		t.Fatalf("control %+v %v", control, err)
	}
	if page, err := svc.CollectionChanges(ctx, c.ID, "", subject); sourceCode(err) != "AST_SOURCE_PAUSED" || page.Cursor != "" {
		t.Fatalf("paused advanced %+v %v", page, err)
	}
	authority.state = "active"
	authority.fail = true
	if _, err := svc.ContentTicketMetadataForSubject(ctx, token, subject); sourceCode(err) != "AST_SOURCE_UNAVAILABLE" {
		t.Fatalf("outage %v", err)
	}
	authority.fail = false
	subjects.fail = true
	if _, err := svc.ContentTicketMetadataForSubject(ctx, token, subject); sourceCode(err) != "AST_SOURCE_UNAVAILABLE" {
		t.Fatalf("Account outage %v", err)
	}
	subjects.fail = false
	for _, r := range authority.requests {
		if r.ACLRevision != 2 || r.BindingGeneration != 1 || r.CollectionID != c.ID {
			t.Fatalf("untrusted context %+v", r)
		}
		if r.Mode == "published" && (r.ItemID != item.ID || r.PublicationRevision != item.SourceRevision || r.Source == nil || !r.Source.Matches(*a.Source)) {
			t.Fatalf("wrong publication %+v", r)
		}
	}
	if _, err := svc.IssueManagedContentTickets(ctx, c.ID, assets.LineHarnessCaller, []string{item.ID}, time.Minute); err == nil {
		t.Fatal("manager ticket bypass")
	}
	now = a.Source.ExpiresAt
	if _, err := svc.AuthorizedCollectionContentMetadata(ctx, c.ID, item.ID, subject); sourceCode(err) != "AST_SOURCE_EXPIRED" {
		t.Fatalf("hard expiry %v", err)
	}
	page, err := svc.CollectionChanges(ctx, c.ID, "", subject)
	if err != nil || len(page.Items) != 0 || len(page.Tombstones) != 1 || page.Tombstones[0].ID != item.ID {
		t.Fatalf("expired metadata %+v %v", page, err)
	}
}
func sourceCode(err error) string {
	var access *assets.SourceAccessError
	if errors.As(err, &access) {
		return access.Code
	}
	return ""
}

func TestSourceExpiryTombstonesCollectionsAndUnattachedOriginals(t *testing.T) {
	store, c, item, a, now := sourceReaderFixture(t)
	ctx := context.Background()
	if changed, err := store.ExpireLineSource(ctx, a.Source.ExpiresAt.Add(-time.Microsecond)); err != nil || changed {
		t.Fatalf("early expiry %v %v", changed, err)
	}
	if changed, err := store.ExpireLineSource(ctx, a.Source.ExpiresAt); err != nil || !changed {
		t.Fatalf("expiry %v %v", changed, err)
	}
	page, err := store.CollectionChanges(ctx, c.ID, "", assets.CollectionSubject{UserID: "reader", RoleIDs: []string{"allowed"}})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("expired reset %+v %v", page, err)
	}
	var deletedRevision int64
	if err := store.db.QueryRow(`SELECT deleted_revision FROM asset_collection_items WHERE id=$1`, item.ID).Scan(&deletedRevision); err != nil || deletedRevision <= item.CreatedRevision {
		t.Fatalf("durable tombstone %d %v", deletedRevision, err)
	}
	original := insertLineSource(t, store.db, "unattached-original", now)
	if changed, err := store.ExpireLineSource(ctx, original.Source.ExpiresAt); err != nil || !changed {
		t.Fatalf("unattached expiry %v %v", changed, err)
	}
	expired, err := store.GetAsset(ctx, original.ID)
	if err != nil || expired.DeletedAt.IsZero() {
		t.Fatalf("unattached source %+v %v", expired, err)
	}
	if changed, err := store.ExpireLineSource(ctx, original.Source.ExpiresAt); err != nil || changed {
		t.Fatalf("expiry replay %v %v", changed, err)
	}
}

func TestSourceReaderProtocolGateDoesNotExposeUnownedCollections(t *testing.T) {
	store, c, item, _, now := sourceReaderFixture(t)
	subjects := &readerCurrentSubjects{roles: []string{"allowed"}}
	svc := assets.NewService(store, nil, "", func() time.Time { return now }).WithSourceAuthority(&readerSourceAuthority{state: "active"}, subjects)
	subject := assets.CollectionSubject{UserID: "reader"}
	_, err := svc.GetAuthorizedCollectionItem(context.Background(), c.ID, item.ID, subject)
	if sourceCode(err) != "AST_SOURCE_CLIENT_UPGRADE" {
		t.Fatalf("old reader: %v", err)
	}
	subject.SourcePolicyVersion = 1
	if _, err = svc.GetAuthorizedCollectionItem(context.Background(), c.ID, item.ID, subject); err != nil {
		t.Fatal(err)
	}
	subject.SourcePolicyVersion = 2
	if _, err = svc.GetAuthorizedCollectionItem(context.Background(), c.ID, item.ID, subject); sourceCode(err) != "AST_SOURCE_CLIENT_UPGRADE" {
		t.Fatalf("future reader: %v", err)
	}
	subjects.roles = nil
	if _, err = svc.GetAuthorizedCollectionItem(context.Background(), c.ID, item.ID, subject); sourceCode(err) != "AST_SOURCE_ACL_REVOKED" {
		t.Fatalf("unowned: %v", err)
	}
}
