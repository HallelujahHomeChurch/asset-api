package assets

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// A sequential per-object status probe cannot complete within the CMS deadline
// for a congregation recording. Listing is remote size evidence, not ready.
type listingPackageObjects struct {
	packageObjects
	heads, lists int
	listErr      error
}

func TestRecordingPackageStatusDoesNotListUnauthorizedOrClosedUploads(t *testing.T) {
	ctx := context.Background()
	svc, repo, _, now, input := newPackageTest(t)
	p, err := svc.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	objects := &listingPackageObjects{}
	svc.objects = objects
	if _, err := svc.Status(ctx, p.ID, "another-actor", "", 1000); !errors.Is(err, ErrForbidden) || objects.lists != 0 {
		t.Fatalf("unauthorized listing: %v calls=%d", err, objects.lists)
	}
	*now = p.ExpiresAt
	if page, err := svc.Status(ctx, p.ID, input.ActorID, "", 1000); err != nil || len(page.ConfirmedObjects) != 0 || objects.lists != 0 {
		t.Fatalf("expired session listed: %v calls=%d", err, objects.lists)
	}
	*now = p.ExpiresAt.Add(-time.Minute)
	p.State = "freezing"
	repo.packages[p.ID] = p
	if page, err := svc.Status(ctx, p.ID, input.ActorID, "", 1000); err != nil || page.State != "freezing" || objects.lists != 0 {
		t.Fatalf("closed session listed: %v calls=%d", err, objects.lists)
	}
}

func TestRecordingPackageStatusTerminalCursorDoesNotQueryStorage(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, input := newPackageTest(t)
	p, err := svc.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	objects := &listingPackageObjects{listErr: errors.New("provider must not be called for an empty page")}
	svc.objects = objects
	page, err := svc.Status(ctx, p.ID, input.ActorID, "master.m3u8", 1000)
	if err != nil || len(page.ConfirmedObjects) != 0 || page.NextCursor != "" || objects.lists != 0 {
		t.Fatalf("terminal cursor queried storage: %v calls=%d", err, objects.lists)
	}
}

func (o *listingPackageObjects) Head(context.Context, string) (int64, string, error) {
	o.heads++
	return 0, "", context.DeadlineExceeded
}

func (o *listingPackageObjects) ListPackageObjects(_ context.Context, id string, maxObjects int) (map[string]int64, error) {
	o.lists++
	if maxObjects != RecordingPackageMaxObjects {
		return nil, errors.New("unbounded package listing")
	}
	if o.listErr != nil {
		return nil, o.listErr
	}
	values := make(map[string]int64)
	prefix := "recordings/packages/" + id + "/staging/"
	for key, size := range o.sizes {
		if strings.HasPrefix(key, prefix) {
			values[strings.TrimPrefix(key, prefix)] = size
		}
	}
	return values, nil
}

func TestRecordingPackageStatusListsLongRecordingWithoutPerObjectRequests(t *testing.T) {
	ctx := context.Background()
	svc, repo, _, _, input := newPackageTest(t)
	p, err := svc.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	// Match the reported package's 519 upload objects plus local inventory file.
	p.Inventory.Objects = []RecordingPackageObject{{Path: "master.m3u8", SizeBytes: 100}}
	for _, name := range []string{"720p", "1080p"} {
		for _, path := range []string{"index.m3u8", "init.mp4"} {
			p.Inventory.Objects = append(p.Inventory.Objects, RecordingPackageObject{Path: name + "/" + path, SizeBytes: 100})
		}
		for segment := range 257 {
			p.Inventory.Objects = append(p.Inventory.Objects, RecordingPackageObject{Path: fmt.Sprintf("%s/seg-%06d.m4s", name, segment), SizeBytes: 9_000_000})
		}
	}
	repo.packages[p.ID] = p
	objects := &listingPackageObjects{packageObjects: packageObjects{sizes: make(map[string]int64)}}
	for _, object := range p.Inventory.Objects {
		objects.sizes[p.StagingKey(object.Path)] = object.SizeBytes
	}
	delete(objects.sizes, p.StagingKey("720p/seg-000001.m4s"))
	objects.sizes[p.StagingKey("1080p/seg-000002.m4s")]--
	objects.sizes[p.StagingKey("720p/seg-999999.m4s")] = 9_000_000 // Undeclared object is not confirmed.
	svc.objects = objects
	page, err := svc.Status(ctx, p.ID, input.ActorID, "", 1000)
	if err != nil {
		t.Fatalf("large status page hit per-object timeout: %v", err)
	}
	if page.State != "uploading" || page.ReadyAt != nil || page.NextCursor != "" || len(page.ConfirmedObjects) != 517 || objects.heads != 0 || objects.lists != 1 {
		t.Fatalf("incorrect remote evidence: confirmed=%d state=%s heads=%d lists=%d", len(page.ConfirmedObjects), page.State, objects.heads, objects.lists)
	}
	for _, absent := range []string{"720p/seg-000001.m4s", "1080p/seg-000002.m4s", "720p/seg-999999.m4s"} {
		if slices.Contains(page.ConfirmedObjects, absent) {
			t.Fatalf("unconfirmed object reported: %s", absent)
		}
	}
	if !slices.IsSorted(page.ConfirmedObjects) {
		t.Fatal("status paths must retain inventory order")
	}
	objects.listErr = errors.New("provider unavailable")
	if _, err := svc.Status(ctx, p.ID, input.ActorID, "", 1000); !errors.Is(err, objects.listErr) {
		t.Fatalf("provider failure inferred missing objects: %v", err)
	}
}
