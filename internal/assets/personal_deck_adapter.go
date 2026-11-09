package assets

import (
	"bytes"
	"context"
	"errors"
	"io"

	"hhc/asset-api/internal/presenterformat"
)

// ValidatePersonalDeck adapts portable-format validation to the personal upload lifecycle.
func ValidatePersonalDeck(ctx context.Context, reader io.Reader) error {
	err := presenterformat.ValidateV1(ctx, reader, PersonalMaxFileSize, func(ctx context.Context, name, mime string, data []byte) error {
		_, err := ValidateMedia(ctx, name, mime, data, bytes.NewReader(data), int64(len(data)))
		return err
	})
	if errors.Is(err, presenterformat.ErrInvalidDeck) {
		return ErrInvalidUpload
	}
	return err
}
