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

func TestLineGroupFilePolicyMatchesSupportedAttachmentFormats(t *testing.T) {
	policy, ok := PolicyFor("line.group.file")
	if !ok {
		t.Fatal("line.group.file policy is missing")
	}
	for _, mime := range []string{
		"application/pdf",
		"image/jpeg",
		"image/png",
		"application/vnd.ms-powerpoint",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.apple.keynote",
		"application/vnd.oasis.opendocument.presentation",
		"application/msword",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.ms-excel",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"text/plain",
		"text/markdown",
	} {
		if !policy.AllowsMIME(mime) {
			t.Errorf("MIME %q is not allowed", mime)
		}
	}
	if policy.AllowsSize("text/plain", (2<<20)+1) {
		t.Fatal("text file exceeded its type limit")
	}
	if !policy.AllowsSize("application/pdf", 25<<20) {
		t.Fatal("PDF should allow the existing 25 MiB limit")
	}
}
