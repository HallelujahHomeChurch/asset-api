package assets

import (
	"regexp"
	"time"
)

const LineHarnessCaller = "hhc-line-harness"
const LineSourceSecurityProfile = "line-source-unscanned-v1"
const SourceLifetime = 90 * 24 * time.Hour

var opaqueSourceID = regexp.MustCompile(`^[A-Za-z0-9_-]{12,128}$`)

type SourceReference struct {
	Ref         string    `json:"ref"`
	Revision    string    `json:"revision"`
	PublishedAt time.Time `json:"publishedAt"`
}

type SourcePolicy struct {
	SourceReference
	Version   int       `json:"version"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (r SourceReference) Valid() bool {
	return opaqueSourceID.MatchString(r.Ref) && opaqueSourceID.MatchString(r.Revision) && !r.PublishedAt.IsZero()
}

func (p SourcePolicy) Valid() bool {
	return p.Version == 1 && p.SourceReference.Valid() && p.ExpiresAt.Equal(p.PublishedAt.Add(SourceLifetime))
}

func (a Asset) IsLineSource() bool {
	return a.OwnerService == LineHarnessCaller && a.UploadProfiles == (UploadProfiles{LineSourceSecurityProfile, noProcessing}) &&
		(a.Namespace == "line.group.file" || a.Namespace == "line.group.media-sync") && a.Source != nil && a.Source.Valid()
}

func (a Asset) SourceAvailable(now time.Time) bool {
	return a.IsLineSource() && !a.Source.PublishedAt.After(now) && now.Before(a.Source.ExpiresAt) && a.DeletedAt.IsZero() && a.PurgedAt.IsZero()
}

func (p SourcePolicy) Matches(other SourcePolicy) bool {
	return p.Version == other.Version && p.Ref == other.Ref && p.Revision == other.Revision && p.PublishedAt.Equal(other.PublishedAt) && p.ExpiresAt.Equal(other.ExpiresAt)
}
