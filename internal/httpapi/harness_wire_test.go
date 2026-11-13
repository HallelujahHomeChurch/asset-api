package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

// This fixture is copied byte-for-byte to Harness; it exercises the producer's real JSON serializers.
func TestHarnessWireFixture(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fixed := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	ref := "11111111-1111-4111-8111-111111111111"
	revision := "22222222-2222-4222-8222-222222222222"
	a := assets.Asset{ID: strings.Repeat("a", 32), Namespace: "line.group.file", OwnerService: assets.LineHarnessCaller, OwnerType: "line_message", OwnerID: ref, Purpose: "original", OriginalFileName: "fixture.pdf", ExpectedMIMEType: "application/pdf", Visibility: assets.VisibilityRestricted, UploadProfiles: assets.UploadProfiles{SecurityProfile: assets.LineSourceSecurityProfile, ProcessingProfile: "no-processing-v1"}, Source: &assets.SourcePolicy{Version: 1, SourceReference: assets.SourceReference{Ref: ref, Revision: revision, PublishedAt: now}, ExpiresAt: now.Add(assets.SourceLifetime)}, UploadStatus: assets.UploadCreated, ScanStatus: assets.ScanPending, ProcessingStatus: assets.ProcessingNotRequired, CreatedAt: now, UpdatedAt: now}
	encode := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	created := assets.CreatedUpload{Asset: a, Session: assets.UploadSession{ID: strings.Repeat("b", 32), AssetID: a.ID, CallerService: assets.LineHarnessCaller}, Target: assets.UploadTarget{URL: "https://blob.invalid/original", Method: "PUT", Headers: map[string]string{"x-ms-blob-type": "BlockBlob"}, ExpiresAt: now.Add(time.Hour)}}
	fixtures := map[string]json.RawMessage{"created": encode(created)}
	a.UploadStatus = assets.UploadCompleted
	a.ScanStatus = assets.ScanNotRequired
	a.SizeBytes = 16
	a.DetectedMIMEType = "application/pdf"
	a.ChecksumSHA256 = strings.Repeat("f", 64)
	a.ETag = "fixture-etag"
	request := httptest.NewRequest("GET", "/priv/assets/"+a.ID, nil)
	request = request.WithContext(context.WithValue(request.Context(), workloadPrincipalKey{}, "harness"))
	recorder := httptest.NewRecorder()
	writePrivateAsset(recorder, request, a)
	fixtures["ready"] = json.RawMessage(recorder.Body.Bytes())
	fixtures["lookup"] = encode(assets.CreatedUpload{Asset: a, Session: created.Session})
	collection := assets.Collection{ID: strings.Repeat("c", 32), Namespace: "line.group.media-sync", Name: "Fixture", ACLRevision: 1, BindingGeneration: 1, SourcePolicyVersion: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
	acl := assets.CollectionACL{ID: strings.Repeat("d", 32), CollectionID: collection.ID, SubjectType: assets.SubjectUser, SubjectID: "33333333-3333-4333-8333-333333333333", Permission: assets.PermissionRead, CreatedAt: now}
	managed := assets.ManagedCollection{Collection: collection, ACLs: []assets.CollectionACL{acl}}
	fixtures["collection"] = encode(managed)
	fixtures["collectionCreated"] = encode(collection)
	fixtures["collections"] = encode(assets.ManagedCollectionPage{Collections: []assets.ManagedCollection{managed}})
	item := assets.CollectionItem{ID: strings.Repeat("e", 32), CollectionID: collection.ID, AssetID: a.ID, RemoteItemID: ref, SourceRevision: "44444444-4444-4444-8444-444444444444", PublicationState: "staged", SourceExpiresAt: a.Source.ExpiresAt, CreatedAt: now, UpdatedAt: now}
	fixtures["staged"] = encode(assets.CollectionItemMutation{Collection: collection, Item: item})
	item.PublicationState = "active"
	fixtures["active"] = encode(assets.CollectionItemMutation{Collection: collection, Item: item})
	data, err := json.MarshalIndent(fixtures, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Freeze only fixture timestamps after the live readiness decision has been serialized.
	for _, delta := range []time.Duration{assets.SourceLifetime, time.Hour, 0} {
		data = []byte(strings.ReplaceAll(string(data), now.Add(delta).Format(time.RFC3339), fixed.Add(delta).Format(time.RFC3339)))
	}
	data = append(data, '\n')
	path := filepath.Join("testdata", "harness-wire.json")
	if os.Getenv("UPDATE_HARNESS_WIRE_FIXTURE") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if json.Unmarshal(expected, &want) != nil || json.Unmarshal(data, &got) != nil || !reflect.DeepEqual(want, got) {
		t.Fatal("Harness wire fixture drift; regenerate and verify the consumer")
	}
}
