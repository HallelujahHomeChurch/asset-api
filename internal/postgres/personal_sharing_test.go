package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestPersonalFolderSharingAuthorizationAndEffectiveRoots(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC()
	_, err := store.EnsurePersonalSpace(ctx, "owner", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []assets.PersonalMutation{
		{OperationID: "parent", Type: "create-folder", ItemID: "parent", Name: "Parent"},
		{OperationID: "child", Type: "create-folder", ItemID: "child", ParentID: "parent", Name: "Child"},
		{OperationID: "sibling", Type: "create-folder", ItemID: "sibling", Name: "Sibling"},
	} {
		if _, err = store.ApplyPersonalMutation(ctx, "owner", mutation, now); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := store.CreatePersonalFolderGrant(ctx, "owner", "parent", "recipient", now)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := store.CreatePersonalFolderGrant(ctx, "owner", "parent", "recipient", now.Add(time.Second))
	if err != nil || duplicate.ID != parent.ID {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	child, err := store.CreatePersonalFolderGrant(ctx, "owner", "child", "recipient", now)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := store.ListSharedFolderRoots(ctx, "recipient")
	if err != nil || len(roots) != 1 || roots[0].GrantID != parent.ID {
		t.Fatalf("roots=%+v err=%v", roots, err)
	}
	page, err := store.SharedFolderSnapshot(ctx, "recipient", parent.ID, "", 100)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("snapshot=%+v err=%v", page, err)
	}
	if _, err = store.SharedFolderSnapshot(ctx, "other", parent.ID, "", 100); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("foreign snapshot: %v", err)
	}
	if err = store.LeaveSharedFolder(ctx, "recipient", parent.ID, now); err != nil {
		t.Fatal(err)
	}
	roots, err = store.ListSharedFolderRoots(ctx, "recipient")
	if err != nil || len(roots) != 1 || roots[0].GrantID != child.ID {
		t.Fatalf("child root=%+v err=%v", roots, err)
	}
	if err = store.RevokePersonalFolderGrant(ctx, "owner", "child", child.ID, now); err != nil {
		t.Fatal(err)
	}
	if err = store.RevokePersonalFolderGrant(ctx, "owner", "child", child.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreatePersonalFolderGrant(ctx, "owner", "sibling", "owner", now); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("self share: %v", err)
	}
}
