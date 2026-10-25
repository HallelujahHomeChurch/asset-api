package assets

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/jackc/pgx/v5/pgconn"
)

type uploadDiagnosticRepository struct {
	*memoryRepository
	stage   string
	failure error
}

func (r uploadDiagnosticRepository) FindUploadByIdempotency(ctx context.Context, caller, operation, key string) (Asset, UploadSession, error) {
	if r.stage == "replay" {
		return Asset{}, UploadSession{}, r.failure
	}
	return r.memoryRepository.FindUploadByIdempotency(ctx, caller, operation, key)
}
func (r uploadDiagnosticRepository) CreateUpload(ctx context.Context, a Asset, u UploadSession) error {
	if r.stage == "persist" {
		return r.failure
	}
	return r.memoryRepository.CreateUpload(ctx, a, u)
}

type uploadDiagnosticBlobs struct {
	*memoryBlobStore
	failure error
}

func (b uploadDiagnosticBlobs) CreateUploadTarget(ctx context.Context, key string, n int64, at time.Time) (UploadTarget, error) {
	if b.failure != nil {
		return UploadTarget{}, b.failure
	}
	return b.memoryBlobStore.CreateUploadTarget(ctx, key, n, at)
}

func TestWeeklyUploadFailureDiagnosticsPreserveOutcomeAndPrivacy(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	private := "https://private.invalid/private-id?sig=private-secret private-source"
	cases := []struct {
		stage, namespace, kind, code string
		failure                      error
	}{
		{"replay", "cms.weekly.image", "postgres", "42P01", &pgconn.PgError{Code: "42P01", Message: private, Detail: private, TableName: "private-table"}},
		{"sign", "cms.weekly.image", "azure", "AuthorizationPermissionMismatch", &azcore.ResponseError{StatusCode: 403, ErrorCode: "AuthorizationPermissionMismatch"}},
		{"persist", "cms.weekly.image", "other", "", errors.New(private)},
		{"persist", "cms.weekly.image", "postgres", "", &pgconn.PgError{Code: private, Message: private}},
		{"sign", "cms.weekly.image", "azure", "other", &azcore.ResponseError{StatusCode: 500, ErrorCode: private}},
		{"success", "cms.weekly.image", "", "", nil},
		{"persist", "cms.weekly.pdf", "", "", errors.New(private)},
	}
	for _, tc := range cases {
		t.Run(tc.stage+tc.namespace+tc.code, func(t *testing.T) {
			output.Reset()
			repo := uploadDiagnosticRepository{newMemoryRepository(), tc.stage, tc.failure}
			blobs := uploadDiagnosticBlobs{memoryBlobStore: newMemoryBlobStore()}
			if tc.stage == "sign" {
				blobs.failure = tc.failure
			}
			service := NewService(repo, blobs, "", time.Now)
			input := CreateUploadInput{Namespace: tc.namespace, OwnerService: "hhc-web-api", OwnerType: "bulletin_issue", OwnerID: "private-id", Purpose: "online_bulletin_illustration", Locale: "zh-Hant", OriginalFileName: "private-source.png", ExpectedMIMEType: "image/png", MaxSizeBytes: 100, Visibility: VisibilityPrivate}
			if tc.namespace == "cms.weekly.pdf" {
				input.ExpectedMIMEType = "application/pdf"
				input.OriginalFileName = "private-source.pdf"
			}
			_, err := service.CreateUploadSession(context.Background(), input, "private-idempotency")
			if tc.failure == nil && err != nil || tc.failure != nil && !errors.Is(err, tc.failure) {
				t.Fatal("returned outcome changed")
			}
			log := output.String()
			if tc.kind == "" {
				if log != "" {
					t.Fatal("success/unrelated namespace logged")
				}
				return
			}
			for _, required := range []string{`"msg":"bulletin_upload_failed"`, `"stage":"` + tc.stage + `"`, `"kind":"` + tc.kind + `"`} {
				if !strings.Contains(log, required) {
					t.Fatalf("missing %s", required)
				}
			}
			if tc.code != "" && !strings.Contains(log, `"code":"`+tc.code+`"`) {
				t.Fatal("missing bounded provider code")
			}
			if tc.kind == "azure" && !strings.Contains(log, `"status":`) {
				t.Fatal("missing provider status")
			}
			for _, secret := range []string{"https://", "private.invalid", "private-id", "private-secret", "private-source", "private-table", "sig="} {
				if strings.Contains(log, secret) {
					t.Fatal("private data leaked")
				}
			}
		})
	}
}
