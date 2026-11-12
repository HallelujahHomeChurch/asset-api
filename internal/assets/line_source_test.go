package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func TestLineSourceProfileAdmissionAndDeadline(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	profiles := UploadProfiles{"line-source-unscanned-v1", "no-processing-v1"}
	for _, namespace := range []string{"line.group.file", "line.group.media-sync"} {
		policy, ok := UploadPolicy(namespace, profiles)
		if !ok || policy.OwnerService != "hhc-line-harness" || policy.AllowsVisibility(VisibilityPublic) {
			t.Fatalf("policy %s: %+v %v", namespace, policy, ok)
		}
		if (UploadProfileScope{NamespaceFamily: namespace, UploadProfiles: profiles}).ValidForCaller("hhc-line-function-bot") {
			t.Fatal("legacy caller admitted")
		}
	}
	for _, ns := range []string{"cms.weekly.pdf", "line.group.file.other", "account.avatar"} {
		if _, ok := UploadPolicy(ns, profiles); ok {
			t.Fatalf("unexpected namespace %s", ns)
		}
	}
	valid := SourceReference{Ref: "source_opaque_123", Revision: "revision_opaque_123", PublishedAt: now.Add(-80 * 24 * time.Hour)}
	for _, tc := range []struct {
		name     string
		source   *SourceReference
		profiles UploadProfiles
		want     error
	}{
		{"valid", &valid, profiles, nil},
		{"missing", nil, profiles, ErrInvalidInput},
		{"expired", &SourceReference{Ref: valid.Ref, Revision: valid.Revision, PublishedAt: now.Add(-90 * 24 * time.Hour)}, profiles, ErrInvalidInput},
		{"future", &SourceReference{Ref: valid.Ref, Revision: valid.Revision, PublishedAt: now.Add(time.Second)}, profiles, ErrInvalidInput},
		{"raw reference", &SourceReference{Ref: "https://line.example/private", Revision: valid.Revision, PublishedAt: valid.PublishedAt}, profiles, ErrInvalidInput},
		{"legacy source forbidden", &valid, UploadProfiles{"line-media-v1", "no-processing-v1"}, ErrInvalidInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMemoryRepository()
			blobs := newMemoryBlobStore()
			svc := NewService(repo, blobs, "", func() time.Time { return now })
			owner := "hhc-line-harness"
			if tc.profiles.SecurityProfile == "line-media-v1" {
				owner = "hhc-line-function-bot"
			}
			input := CreateUploadInput{Namespace: "line.group.media-sync", OwnerService: owner, OwnerType: "line_message", OwnerID: "opaque", Purpose: "original", OriginalFileName: "file.pdf", ExpectedMIMEType: "application/pdf", MaxSizeBytes: 1024, UploadProfiles: tc.profiles, Source: tc.source}
			result, err := svc.CreateUploadSession(context.Background(), input, "operation")
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if err == nil && (result.Asset.Source == nil || result.Asset.Source.Version != 1 || !result.Asset.Source.ExpiresAt.Equal(valid.PublishedAt.Add(90*24*time.Hour))) {
				t.Fatalf("source deadline: %+v", result.Asset.Source)
			}
		})
	}
}

func TestLineSourceCompletionValidatesBytesWithoutScan(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	svc := NewService(repo, blobs, "", func() time.Time { return now })
	created, err := svc.CreateUploadSession(context.Background(), CreateUploadInput{Namespace: "line.group.file", OwnerService: "hhc-line-harness", OwnerType: "line_message", OwnerID: "opaque", Purpose: "original", OriginalFileName: "file.pdf", ExpectedMIMEType: "application/pdf", MaxSizeBytes: 1024, UploadProfiles: UploadProfiles{"line-source-unscanned-v1", "no-processing-v1"}, Source: &SourceReference{Ref: "source_opaque_123", Revision: "revision_opaque_123", PublishedAt: now}}, "operation")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("%PDF-1.7\nsynthetic source")
	blobs.objects[created.Session.StagingObjectKey] = payload
	sum := sha256.Sum256(payload)
	completed, err := svc.CompleteUpload(context.Background(), created.Asset.ID, CompleteUploadInput{SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "application/pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.scanRequests) != 0 {
		t.Fatal("unscanned source queued for scanning")
	}
	if completed.ScanStatus != ScanNotRequired || completed.ScanSignature != "" || completed.Source == nil {
		t.Fatalf("untruthful evidence %+v", completed)
	}
	if _, err := svc.AuthorizedMetadata(context.Background(), completed.ID, SubjectUser, "reader"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy grant bypass: %v", err)
	}
	now = now.Add(90 * 24 * time.Hour)
	if _, err := svc.CompleteUpload(context.Background(), created.Asset.ID, CompleteUploadInput{SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "application/pdf"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired completion: %v", err)
	}
}
