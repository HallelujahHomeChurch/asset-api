package auditclient

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

const (
	userID = "018f2f5b-8b6d-4a7d-9f2e-7a6b5c4d3e2f"
	hexID  = "0123456789abcdef0123456789abcdef"
)

func TestFixtureAndEveryContractExampleValidate(t *testing.T) {
	fixture, err := LoadFixture()
	if err != nil {
		t.Fatal(err)
	}
	contracts := fixture.Contracts()
	if len(contracts) != 19 {
		t.Fatalf("contracts=%d", len(contracts))
	}
	for _, contract := range contracts {
		resource := hexID
		if contract.ResourceIDGrammar == "empty" {
			resource = ""
		}
		metadata := exampleMetadata(contract.MetadataSchema)
		actorType, actor := "user", userID
		if contract.Action == "asset.collection_item.add" {
			actorType, actor = "service", "hhc-line-function-bot"
		}
		if _, err := NewEvent(contract.Action, resource, actorType, actor, "request-1", time.Unix(1, 0), metadata); err != nil {
			t.Fatalf("%s: %v", contract.Action, err)
		}
	}
}

func TestActorAndRequestValidationFailClosed(t *testing.T) {
	metadata := Metadata{"collectionId": hexID}
	for _, test := range []struct{ actorType, actor, request string }{
		{"service", "other-service", "request-1"},
		{"service", "hhc-line-function-bot", "request-1"},
		{"user", userID, "eyJhbGciOiJub25lIn0.eyJzdWIiOiIxIn0.c2ln"},
	} {
		_, err := NewEvent("asset.collection_item.rename", hexID, test.actorType, test.actor, test.request, time.Unix(1, 0), metadata)
		if !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("accepted %#v: %v", test, err)
		}
	}
	if !ValidUserRequest(userID, "request-1") || ValidUserRequest("bad", "request-1") {
		t.Fatal("user provenance validation mismatch")
	}
}

func TestCanonicalRoundTripAndAccessReplayIdentity(t *testing.T) {
	first, err := NewEvent("asset.collection.read", hexID, "user", userID, "request-1", time.Unix(1, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewEvent("asset.collection.read", hexID, "user", userID, "request-1", time.Unix(2, 0), Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	if first.EventID != second.EventID || !EquivalentForEnqueue(first, second) {
		t.Fatal("access replay identity mismatch")
	}
	payload, _, err := first.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(payload)
	if err != nil || decoded.EventID != first.EventID {
		t.Fatalf("round trip: %v", err)
	}
	payload = append(payload[:len(payload)-1], []byte(`,"extra":true}`)...)
	if _, err := Decode(payload); !errors.Is(err, ErrInvalidEvent) {
		t.Fatal("unknown field accepted")
	}
}

func exampleMetadata(schema string) Metadata {
	switch schema {
	case "empty", "filters":
		return Metadata{}
	case "collection_acl":
		return Metadata{"collectionId": hexID, "subjectType": "role"}
	case "collection_parent":
		return Metadata{"collectionId": hexID}
	case "ticket_count":
		return Metadata{"normalizedUniqueItemCount": 1}
	case "items_deleted":
		return Metadata{"normalizedUniqueItemCount": 1, "deletedCount": 1, "alreadyAbsentCount": 0}
	case "items_updated":
		return Metadata{"normalizedUniqueItemCount": 1, "updatedCount": 1, "alreadyAbsentCount": 0}
	case "upload":
		return Metadata{"namespace": "cms.weekly.pdf", "ownerType": "bulletin_issue", "purpose": "weekly_bulletin"}
	default:
		panic(schema)
	}
}

func TestCountsRejectFractionalJSONNumber(t *testing.T) {
	if validMetadata("ticket_count", Metadata{"normalizedUniqueItemCount": json.Number("1.5")}) {
		t.Fatal("fractional count accepted")
	}
}
