package azure

import (
	"errors"
	"net/http"
	"testing"

	"hhc/asset-api/internal/assets"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

func TestContentRangeTotal(t *testing.T) {
	if total := contentRangeTotal("bytes 10-19/100", 10); total != 100 {
		t.Fatalf("total = %d", total)
	}
	if total := contentRangeTotal("", 10); total != 10 {
		t.Fatalf("fallback total = %d", total)
	}
}

func TestMapErrorClassifiesBlobPreconditions(t *testing.T) {
	for status, expected := range map[int]error{
		http.StatusNotFound:                     assets.ErrNotFound,
		http.StatusConflict:                     assets.ErrConflict,
		http.StatusPreconditionFailed:           assets.ErrInvalidUpload,
		http.StatusRequestedRangeNotSatisfiable: assets.ErrInvalidInput,
	} {
		if err := mapError(&azcore.ResponseError{StatusCode: status}); !errors.Is(err, expected) {
			t.Fatalf("status %d mapped to %v", status, err)
		}
	}
}
