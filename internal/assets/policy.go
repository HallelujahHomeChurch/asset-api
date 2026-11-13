package assets

import (
	"regexp"
	"strings"
)

type NamespacePolicy struct {
	OwnerService      string
	MIMETypes         map[string]bool
	MIMEMaxSizeBytes  map[string]int64
	MaxSizeBytes      int64
	DefaultVisibility Visibility
	Visibilities      map[Visibility]bool
	Processing        ProcessingStatus
	CacheControl      string
	Width             int
	Height            int
}

func (p NamespacePolicy) AllowsMIME(value string) bool           { return p.MIMETypes[value] }
func (p NamespacePolicy) AllowsVisibility(value Visibility) bool { return p.Visibilities[value] }
func (p NamespacePolicy) AllowsSize(mime string, size int64) bool {
	limit := p.MaxSizeBytes
	if typed, ok := p.MIMEMaxSizeBytes[mime]; ok {
		limit = typed
	}
	return size > 0 && size <= limit
}

const (
	noProcessing  = "no-processing-v1"
	imageVariants = "image-variants-v1"
)

var legacyUploadProfiles = map[string]UploadProfiles{
	"account.avatar":        {"avatar-jpeg-v1", noProcessing},
	"cms.weekly.pdf":        {"bulletin-pdf-v1", noProcessing},
	"cms.weekly.image":      {"bulletin-png-v1", noProcessing},
	"cms.news.cover":        {"web-image-v1", imageVariants},
	"cms.home.banner":       {"home-banner-jpeg-v1", noProcessing},
	"cms.page.image":        {"web-image-v1", imageVariants},
	"line.group.file":       {"line-attachment-v1", noProcessing},
	PersonalNamespace:       {"personal-media-v1", noProcessing},
	"line.group.media-sync": {"line-media-v1", noProcessing},
}

var uploadProfilePolicies = map[UploadProfiles]NamespacePolicy{
	{LineSourceSecurityProfile, noProcessing}: {
		OwnerService: LineHarnessCaller,
		MIMETypes: map[string]bool{
			"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true, "image/bmp": true,
			"application/pdf": true, "application/vnd.ms-excel": true, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": true,
			"video/mp4": true, "video/quicktime": true, "video/webm": true, "video/ogg": true, "video/x-msvideo": true, "video/x-matroska": true, "video/x-ms-wmv": true,
			"audio/mpeg": true, "audio/wav": true, "audio/mp4": true, "audio/aac": true, "audio/ogg": true,
		},
		MIMEMaxSizeBytes: map[string]int64{"application/pdf": 25 << 20, "application/vnd.ms-excel": 25 << 20, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": 25 << 20, "image/jpeg": 10 << 20, "image/png": 10 << 20, "image/gif": 10 << 20, "image/webp": 10 << 20, "image/bmp": 10 << 20},
		MaxSizeBytes:     200 << 20, DefaultVisibility: VisibilityRestricted, Visibilities: map[Visibility]bool{VisibilityRestricted: true}, Processing: ProcessingNotRequired, CacheControl: "private, no-store",
	},
	{"avatar-jpeg-v1", noProcessing}: {
		OwnerService: "account-api", MIMETypes: map[string]bool{"image/jpeg": true},
		MaxSizeBytes: 1 << 20, DefaultVisibility: VisibilityPublic, Visibilities: map[Visibility]bool{VisibilityPublic: true}, Processing: ProcessingNotRequired,
		CacheControl: "public, max-age=31536000, immutable",
	},
	{"bulletin-pdf-v1", noProcessing}: {
		OwnerService: "hhc-web-api", MIMETypes: map[string]bool{"application/pdf": true},
		MaxSizeBytes: 20 << 20, DefaultVisibility: VisibilityPrivate, Visibilities: map[Visibility]bool{VisibilityPrivate: true}, Processing: ProcessingNotRequired,
		CacheControl: "private, no-store",
	},
	{"bulletin-png-v1", noProcessing}: {
		OwnerService: "hhc-web-api", MIMETypes: map[string]bool{"image/png": true},
		MaxSizeBytes: 2 << 20, DefaultVisibility: VisibilityPrivate, Visibilities: map[Visibility]bool{VisibilityPrivate: true}, Processing: ProcessingNotRequired,
		CacheControl: "private, no-store",
	},
	{"web-image-v1", imageVariants}: {
		OwnerService: "hhc-web-api", MIMETypes: map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true},
		MaxSizeBytes: 10 << 20, DefaultVisibility: VisibilityPrivate, Visibilities: map[Visibility]bool{VisibilityPrivate: true, VisibilityPublic: true}, Processing: ProcessingPending,
		CacheControl: "public, max-age=31536000, immutable",
	},
	{"home-banner-jpeg-v1", noProcessing}: {
		OwnerService: "hhc-web-api", MIMETypes: map[string]bool{"image/jpeg": true},
		MaxSizeBytes: 10 << 20, DefaultVisibility: VisibilityPublic, Visibilities: map[Visibility]bool{VisibilityPublic: true}, Processing: ProcessingNotRequired,
		CacheControl: "public, max-age=31536000, immutable", Width: 1900, Height: 700,
	},
	{"line-attachment-v1", noProcessing}: {
		OwnerService: "hhc-line-function-bot",
		MIMETypes: map[string]bool{
			"application/pdf": true, "image/jpeg": true, "image/png": true, "image/webp": true,
			"application/vnd.ms-powerpoint":                                             true,
			"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
			"application/vnd.apple.keynote":                                             true,
			"application/vnd.oasis.opendocument.presentation":                           true,
			"application/msword":                                                        true,
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   true,
			"application/vnd.ms-excel":                                                  true,
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         true,
			"text/plain": true, "text/markdown": true,
		},
		MIMEMaxSizeBytes: map[string]int64{
			"image/jpeg": 10 << 20, "image/png": 10 << 20, "image/webp": 10 << 20,
			"text/plain": 2 << 20, "text/markdown": 2 << 20,
		},
		MaxSizeBytes: 25 << 20, DefaultVisibility: VisibilityRestricted, Visibilities: map[Visibility]bool{VisibilityRestricted: true}, Processing: ProcessingNotRequired,
		CacheControl: "private, no-store",
	},
	{"personal-media-v1", noProcessing}: {
		OwnerService: PersonalNamespace,
		MIMETypes: map[string]bool{
			"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true, "image/bmp": true,
			"video/mp4": true, "video/quicktime": true, "video/webm": true, "video/ogg": true,
			"video/x-msvideo": true, "video/x-matroska": true, "video/x-ms-wmv": true,
			"audio/mpeg": true, "audio/wav": true, "audio/mp4": true, "audio/aac": true, "audio/ogg": true,
			"application/pdf": true,
			"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
			"application/vnd.hhc.presenter+json":                                        true,
		},
		MaxSizeBytes:      200 << 20,
		DefaultVisibility: VisibilityPrivate,
		Visibilities:      map[Visibility]bool{VisibilityPrivate: true},
		Processing:        ProcessingNotRequired,
		CacheControl:      "private, no-store",
	},
	{"line-media-v1", noProcessing}: {
		OwnerService: "hhc-line-function-bot",
		MIMETypes: map[string]bool{
			"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true, "image/bmp": true,
			"video/mp4": true, "video/quicktime": true, "video/webm": true, "video/ogg": true,
			"video/x-msvideo": true, "video/x-matroska": true, "video/x-ms-wmv": true,
			"audio/mpeg": true, "audio/wav": true, "audio/mp4": true, "audio/aac": true, "audio/ogg": true,
			"application/pdf": true,
			"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
			"application/vnd.hhc.presenter+json":                                        true,
		},
		MaxSizeBytes:      200 << 20,
		DefaultVisibility: VisibilityRestricted,
		Visibilities:      map[Visibility]bool{VisibilityRestricted: true},
		Processing:        ProcessingNotRequired,
		CacheControl:      "private, no-store",
	},
}

func PolicyFor(namespace string) (NamespacePolicy, bool) {
	return UploadPolicy(namespace, UploadProfiles{})
}

var uploadNamespacePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}(\.[a-z][a-z0-9-]{0,31}){1,4}$`)

func UploadPolicy(namespace string, profiles UploadProfiles) (NamespacePolicy, bool) {
	if profiles == (UploadProfiles{LineSourceSecurityProfile, noProcessing}) {
		policy, ok := uploadProfilePolicies[profiles]
		return policy, ok && (namespace == "line.group.file" || namespace == "line.group.media-sync")
	}
	legacy, mapped := legacyUploadProfiles[namespace]
	if profiles == (UploadProfiles{}) {
		profiles = legacy
	} else if !uploadNamespacePattern.MatchString(namespace) || (mapped && profiles != legacy) {
		return NamespacePolicy{}, false
	}
	policy, ok := uploadProfilePolicies[profiles]
	// Existing Account and LINE cleanup/retention still bind their exact namespaces.
	if !mapped && (policy.OwnerService != "hhc-web-api" || !strings.HasPrefix(namespace, "cms.")) {
		return NamespacePolicy{}, false
	}
	return policy, ok
}

func policyForAsset(asset Asset) (NamespacePolicy, bool) {
	policy, ok := UploadPolicy(asset.Namespace, asset.UploadProfiles)
	return policy, ok && (asset.UploadProfiles == (UploadProfiles{}) || policy.OwnerService == asset.OwnerService)
}

type UploadProfileScope struct {
	NamespaceFamily string `json:"namespaceFamily"`
	UploadProfiles
}

func (scope UploadProfileScope) ValidForCaller(caller string) bool {
	policy, ok := uploadProfilePolicies[scope.UploadProfiles]
	if !ok || policy.OwnerService != caller || !uploadNamespacePattern.MatchString(scope.NamespaceFamily) {
		return false
	}
	if caller == "hhc-web-api" {
		return strings.HasPrefix(scope.NamespaceFamily, "cms.")
	}
	if caller == LineHarnessCaller {
		_, ok := UploadPolicy(scope.NamespaceFamily, scope.UploadProfiles)
		return ok
	}
	return legacyUploadProfiles[scope.NamespaceFamily] == scope.UploadProfiles
}

func (scope UploadProfileScope) Allows(namespace string, profiles UploadProfiles) bool {
	return scope.UploadProfiles == profiles &&
		(namespace == scope.NamespaceFamily || strings.HasPrefix(namespace, scope.NamespaceFamily+"."))
}
