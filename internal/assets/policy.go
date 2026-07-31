package assets

type NamespacePolicy struct {
	OwnerService      string
	MIMETypes         map[string]bool
	MaxSizeBytes      int64
	DefaultVisibility Visibility
	Visibilities      map[Visibility]bool
	Processing        ProcessingStatus
	CacheControl      string
}

func (p NamespacePolicy) AllowsMIME(value string) bool           { return p.MIMETypes[value] }
func (p NamespacePolicy) AllowsVisibility(value Visibility) bool { return p.Visibilities[value] }

var namespacePolicies = map[string]NamespacePolicy{
	"account.avatar": {
		OwnerService: "account-api", MIMETypes: map[string]bool{"image/jpeg": true},
		MaxSizeBytes: 1 << 20, DefaultVisibility: VisibilityPublic, Visibilities: map[Visibility]bool{VisibilityPublic: true}, Processing: ProcessingNotRequired,
		CacheControl: "public, max-age=300, must-revalidate",
	},
	"cms.weekly.pdf": {
		OwnerService: "hhc-web-api", MIMETypes: map[string]bool{"application/pdf": true},
		MaxSizeBytes: 20 << 20, DefaultVisibility: VisibilityPrivate, Visibilities: map[Visibility]bool{VisibilityPrivate: true, VisibilityPublic: true}, Processing: ProcessingNotRequired,
		CacheControl: "public, max-age=300, must-revalidate",
	},
	"cms.news.cover": {
		OwnerService: "hhc-web-api", MIMETypes: map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true},
		MaxSizeBytes: 10 << 20, DefaultVisibility: VisibilityPrivate, Visibilities: map[Visibility]bool{VisibilityPrivate: true, VisibilityPublic: true}, Processing: ProcessingPending,
		CacheControl: "public, max-age=300, must-revalidate",
	},
	"cms.page.image": {
		OwnerService: "hhc-web-api", MIMETypes: map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true},
		MaxSizeBytes: 10 << 20, DefaultVisibility: VisibilityPrivate, Visibilities: map[Visibility]bool{VisibilityPrivate: true, VisibilityPublic: true}, Processing: ProcessingPending,
		CacheControl: "public, max-age=300, must-revalidate",
	},
	"line.group.file": {
		OwnerService: "hhc-line-function-bot",
		MIMETypes: map[string]bool{
			"application/pdf": true, "image/jpeg": true, "image/png": true, "image/webp": true,
		},
		MaxSizeBytes: 25 << 20, DefaultVisibility: VisibilityRestricted, Visibilities: map[Visibility]bool{VisibilityRestricted: true}, Processing: ProcessingNotRequired,
		CacheControl: "private, no-store",
	},
}

func PolicyFor(namespace string) (NamespacePolicy, bool) {
	policy, ok := namespacePolicies[namespace]
	return policy, ok
}
