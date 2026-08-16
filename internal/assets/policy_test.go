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

func TestLineGroupMediaSyncPolicy(t *testing.T) {
	policy, ok := PolicyFor("line.group.media-sync")
	if !ok {
		t.Fatal("line.group.media-sync policy is missing")
	}
	if policy.OwnerService != "hhc-line-function-bot" || policy.DefaultVisibility != VisibilityRestricted || policy.Processing != ProcessingNotRequired {
		t.Fatalf("unexpected policy: %+v", policy)
	}
	allowed := []string{
		"image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp",
		"video/mp4", "video/quicktime", "video/webm", "video/ogg", "video/x-msvideo", "video/x-matroska", "video/x-ms-wmv",
		"audio/mpeg", "audio/wav", "audio/mp4", "audio/aac", "audio/ogg",
		"application/pdf", "application/vnd.openxmlformats-officedocument.presentationml.presentation", "application/vnd.librepresenter.presentation+json",
	}
	for _, mime := range allowed {
		if !policy.AllowsMIME(mime) {
			t.Errorf("MIME %q is not allowed", mime)
		}
		if !policy.AllowsSize(mime, 200<<20) || policy.AllowsSize(mime, (200<<20)+1) {
			t.Errorf("MIME %q does not enforce the 200 MiB boundary", mime)
		}
	}
	for _, mime := range []string{
		"image/svg+xml", "application/vnd.ms-powerpoint", "application/vnd.apple.keynote",
		"application/vnd.oasis.opendocument.presentation", "video/mpeg", "image/tiff", "image/heic", "image/heif",
	} {
		if policy.AllowsMIME(mime) {
			t.Errorf("MIME %q must be rejected", mime)
		}
	}
}
