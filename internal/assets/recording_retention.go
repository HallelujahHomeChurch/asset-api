package assets

import "time"

type RecordingRetentionPolicy struct {
	RetentionDays int        `json:"retentionDays"`
	Revision      int64      `json:"revision"`
	ActivatedAt   *time.Time `json:"activatedAt"`
}

type RecordingRetentionPreview struct {
	PreviewID     string    `json:"previewId"`
	RetentionDays int       `json:"retentionDays"`
	Revision      int64     `json:"revision"`
	EvaluatedAt   time.Time `json:"evaluatedAt"`
	AffectedCount int64     `json:"affectedCount"`
	AffectedBytes int64     `json:"affectedBytes"`
}

type UpdateRecordingRetentionInput struct {
	RetentionDays    int    `json:"retentionDays"`
	ExpectedRevision int64  `json:"expectedRevision"`
	PreviewID        string `json:"previewId"`
	IdempotencyKey   string `json:"-"`
}

type RecordingLifecycleBinding struct {
	RecordingID string `json:"recordingId"`
	PackageID   string `json:"packageId"`
}

type RecordingLifecycleSnapshot struct {
	Policy RecordingRetentionPolicy `json:"policy"`
	Items  []RecordingPackageStatus `json:"items"`
}
