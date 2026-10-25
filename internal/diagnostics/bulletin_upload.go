package diagnostics

import (
	"context"
	"errors"
	"log/slog"
	"regexp"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/jackc/pgx/v5/pgconn"
)

type UploadStage uint8

const (
	UploadReplay UploadStage = iota
	UploadSign
	UploadPersist
)

var uploadStageNames = [...]string{"replay", "sign", "persist"}
var uploadSQLState = regexp.MustCompile(`^[0-9A-Z]{5}$`)

// BulletinUploadFailure never records raw provider errors or request data.
func BulletinUploadFailure(ctx context.Context, stage UploadStage, err error) {
	if err == nil || int(stage) >= len(uploadStageNames) {
		return
	}
	attrs := []any{"stage", uploadStageNames[stage]}
	var pg *pgconn.PgError
	var azure *azcore.ResponseError
	switch {
	case errors.As(err, &pg):
		attrs = append(attrs, "kind", "postgres")
		if uploadSQLState.MatchString(pg.Code) {
			attrs = append(attrs, "code", pg.Code)
		}
	case errors.As(err, &azure):
		status := azure.StatusCode
		if status < 100 || status > 599 {
			status = 0
		}
		code := "other"
		switch azure.ErrorCode {
		case "AuthorizationPermissionMismatch", "AuthorizationFailure", "AuthenticationFailed", "InvalidAuthenticationInfo", "InvalidQueryParameterValue", "InvalidHeaderValue", "InvalidXmlDocument", "AuthorizationSourceIPMismatch", "InsufficientAccountPermissions", "ServerBusy", "InternalError":
			code = azure.ErrorCode
		}
		attrs = append(attrs, "kind", "azure", "status", status, "code", code)
	default:
		attrs = append(attrs, "kind", "other")
	}
	slog.WarnContext(ctx, "bulletin_upload_failed", attrs...)
}
