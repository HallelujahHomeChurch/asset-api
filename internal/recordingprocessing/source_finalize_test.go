package recordingprocessing

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

type finalizeObjects struct {
	rangeObjects
	copyCalls int
	state     string
}

func (o *finalizeObjects) CopyRecordingSource(_ context.Context, p assets.RecordingSource, attempt string) (assets.RecordingSourceCopy, error) {
	o.copyCalls++
	state := o.state
	if state == "pending" && o.copyCalls > 1 {
		state = "success"
	}
	return assets.RecordingSourceCopy{State: state, Key: "recording-sources/" + p.ID + "/final/" + attempt + "/source", CopyID: "copy-a", ETag: "verified-etag", SizeBytes: int64(len(o.data))}, nil
}

func TestFinalizeSourceWaitsForCopyAndVerifiesHashBeforeEncode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o := &finalizeObjects{rangeObjects: rangeObjects{data: []byte("media")}, state: "pending"}
	p := assets.RecordingSource{ID: strings.Repeat("a", 32), SizeBytes: 5, ChecksumSHA256: fmt.Sprintf("%x", sha256.Sum256(o.data))}
	attempt := strings.Repeat("b", 32)
	got, err := FinalizeSource(ctx, p, attempt, o)
	if err != nil || got.State != "success" || o.copyCalls != 2 || len(o.ranges) != 1 {
		t.Fatalf("finalize %+v %v calls=%d", got, err, o.copyCalls)
	}
	p.ChecksumSHA256 = strings.Repeat("0", 64)
	if _, err := FinalizeSource(ctx, p, attempt, o); !errors.Is(err, assets.ErrInvalidUpload) {
		t.Fatalf("hash mismatch accepted: %v", err)
	}
	o.state = "failed"
	o.ranges = nil
	if _, err := FinalizeSource(ctx, p, attempt, o); !errors.Is(err, ErrSourceCopyFailed) || len(o.ranges) != 0 {
		t.Fatalf("read failed copy: %v", err)
	}
}
