package assets

import "testing"

func TestAccountAvatarPolicy(t *testing.T) {
	policy, ok := PolicyFor("account.avatar")
	if !ok {
		t.Fatal("account.avatar policy is missing")
	}
	if policy.OwnerService != "account-api" {
		t.Fatalf("owner service = %q", policy.OwnerService)
	}
	if policy.MaxSizeBytes != 1<<20 {
		t.Fatalf("max size = %d", policy.MaxSizeBytes)
	}
	if !policy.AllowsMIME("image/jpeg") || policy.AllowsMIME("image/png") {
		t.Fatalf("unexpected MIME policy: %#v", policy.MIMETypes)
	}
	if policy.Processing != ProcessingNotRequired {
		t.Fatalf("processing = %q", policy.Processing)
	}
}

func TestFutureDesktopNamespaceIsNotActive(t *testing.T) {
	if _, ok := PolicyFor("desktop.cloud-folder.object"); ok {
		t.Fatal("future desktop namespace must not be active")
	}
}
