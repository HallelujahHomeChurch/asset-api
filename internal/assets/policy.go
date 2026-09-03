package assets

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

var namespacePolicies = map[string]NamespacePolicy{
	"account.avatar": {
		OwnerService: "account-api", MIMETypes: map[string]bool{"image/jpeg": true},
		MaxSizeBytes: 1 << 20, DefaultVisibility: VisibilityPublic, Visibilities: map[Visibility]bool{VisibilityPublic: true}, Processing: ProcessingNotRequired,
		CacheControl: "public, max-age=31536000, immutable",
	},
	"account.dsr-export": {
		OwnerService: "account-api", MIMETypes: map[string]bool{"application/zip": true},
		MaxSizeBytes: 50 << 20, DefaultVisibility: VisibilityPrivate, Visibilities: map[Visibility]bool{VisibilityPrivate: true}, Processing: ProcessingNotRequired,
		CacheControl: "private, no-store",
	},
	"cms.weekly.pdf": {
		OwnerService: "hhc-web-api", MIMETypes: map[string]bool{"application/pdf": true},
		MaxSizeBytes: 20 << 20, DefaultVisibility: VisibilityPrivate, Visibilities: map[Visibility]bool{VisibilityPrivate: true, VisibilityPublic: true}, Processing: ProcessingNotRequired,
		CacheControl: "public, max-age=31536000, immutable",
	},
	"cms.news.cover": {
		OwnerService: "hhc-web-api", MIMETypes: map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true},
		MaxSizeBytes: 10 << 20, DefaultVisibility: VisibilityPrivate, Visibilities: map[Visibility]bool{VisibilityPrivate: true, VisibilityPublic: true}, Processing: ProcessingPending,
		CacheControl: "public, max-age=31536000, immutable",
	},
	"cms.home.banner": {
		OwnerService: "hhc-web-api", MIMETypes: map[string]bool{"image/jpeg": true},
		MaxSizeBytes: 10 << 20, DefaultVisibility: VisibilityPublic, Visibilities: map[Visibility]bool{VisibilityPublic: true}, Processing: ProcessingNotRequired,
		CacheControl: "public, max-age=31536000, immutable", Width: 1900, Height: 700,
	},
	"cms.page.image": {
		OwnerService: "hhc-web-api", MIMETypes: map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true},
		MaxSizeBytes: 10 << 20, DefaultVisibility: VisibilityPrivate, Visibilities: map[Visibility]bool{VisibilityPrivate: true, VisibilityPublic: true}, Processing: ProcessingPending,
		CacheControl: "public, max-age=31536000, immutable",
	},
	"line.group.file": {
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
	"line.group.media-sync": {
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
	policy, ok := namespacePolicies[namespace]
	return policy, ok
}
