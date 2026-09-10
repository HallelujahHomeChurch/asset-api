package assets

import (
	"errors"
	"testing"
)

func TestPersonalTrashPurgeValidation(t *testing.T) {
	valid := []PersonalTrashPurgeInput{
		{OperationID: "purge-one", ItemIDs: []string{"item"}},
		{OperationID: "purge-all", All: true},
	}
	for _, input := range valid {
		if err := input.Validate(); err != nil {
			t.Fatalf("valid input %+v: %v", input, err)
		}
	}
	invalid := []PersonalTrashPurgeInput{
		{},
		{OperationID: "both", ItemIDs: []string{"item"}, All: true},
		{OperationID: "empty"},
		{OperationID: "duplicate", ItemIDs: []string{"item", "item"}},
	}
	for _, input := range invalid {
		if !errors.Is(input.Validate(), ErrInvalidInput) {
			t.Fatalf("invalid input accepted: %+v", input)
		}
	}
}

func TestPersonalQuotaExceededError(t *testing.T) {
	err := &PersonalQuotaExceeded{UsedBytes: 90, QuotaBytes: 100, RequiredBytes: 20}
	if !errors.Is(err, ErrPersonalQuotaExceeded) {
		t.Fatal("typed quota error must match sentinel")
	}
}

func TestPersonalMutationValidation(t *testing.T) {
	for _, name := range []string{"", " ", "a/b", "a\\b", "a\x00b", "a\nb"} {
		m := PersonalMutation{OperationID: "op", Type: "create-folder", ItemID: "node", Name: name}
		if m.Validate() == nil {
			t.Errorf("accepted invalid name %q", name)
		}
	}
	if err := (PersonalMutation{OperationID: "op", Type: "create-folder", ItemID: "node", Name: "簡報"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (PersonalMutation{OperationID: "op", Type: "rename", ItemID: "node", Name: "deck"}).Validate(); err == nil {
		t.Fatal("accepted missing revision")
	}
}

func TestPersonalRestoreNameValidation(t *testing.T) {
	for _, name := range []string{" ", "a/b", "a\\b", "a\x00b"} {
		if err := (PersonalMutation{OperationID: "op", Type: "restore", ItemID: "node", Name: name, ExpectedRevision: 1}).Validate(); err == nil {
			t.Errorf("accepted restore name %q", name)
		}
	}
	for _, name := range []string{"", "Sunday restored"} {
		if err := (PersonalMutation{OperationID: "op", Type: "restore", ItemID: "node", Name: name, ExpectedRevision: 1}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
}
