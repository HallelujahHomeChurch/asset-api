package assets

import "testing"

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
