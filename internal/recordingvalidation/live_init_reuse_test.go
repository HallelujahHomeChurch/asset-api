package recordingvalidation

import (
	"bytes"
	"context"
	"errors"
	"hhc/asset-api/internal/storage/r2"
	"io"
	"testing"

	"hhc/asset-api/internal/assets"
)

func TestLiveInitReuseRequiresCurrentExactEvidenceAndRehashesBytes(t *testing.T) {
	p, objects, declarations := liveIntakeFixture()
	init := declarations[0]
	receipt := assets.RecordingLiveValidation{Version: liveValidationVersion, Init: init, InitETag: "published-version"}
	probe := PackageMediaProbe{Objects: objects, LiveInitEvidence: map[string]assets.RecordingLiveValidation{"1080p": receipt}}
	if _, ok := probe.reusableLiveInit("1080p", init); !ok {
		t.Fatal("matching receipt rejected")
	}
	changed := init
	changed.SizeBytes++
	if _, ok := probe.reusableLiveInit("1080p", changed); ok {
		t.Fatal("changed init accepted")
	}
	for _, version := range []string{"", "future"} {
		copy := receipt
		copy.Version = version
		probe.LiveInitEvidence["1080p"] = copy
		if _, ok := probe.reusableLiveInit("1080p", init); ok {
			t.Fatal("unknown validator accepted")
		}
	}
	probe.LiveInitEvidence["1080p"] = receipt
	key := "recordings/captures/" + p.ID + "/final/" + init.Path
	objects.bytes[key] = bytes.Clone(objects.bytes[p.StagingKey(init.Path)])
	var output bytes.Buffer
	if _, err := probe.copyLiveInit(context.Background(), p.ID, init, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), objects.bytes[key]) {
		t.Fatal("decoder did not receive verified bytes")
	}
	objects.bytes[key][0] ^= 1
	output.Reset()
	if _, err := probe.copyLiveInit(context.Background(), p.ID, init, &output); !errors.Is(err, assets.ErrInvalidUpload) {
		t.Fatal("changed stable bytes trusted", err)
	}
}

type missingLiveInitObjects struct {
	PackageObjects
	key string
}

func (s missingLiveInitObjects) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == s.key {
		return nil, r2.ErrNotFound
	}
	return s.PackageObjects.Open(ctx, key)
}
