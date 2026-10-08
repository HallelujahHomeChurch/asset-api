package assets

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

func packageFixture() RecordingPackageInventory {
	return RecordingPackageInventory{
		SchemaVersion: 1, PresetVersion: "hls-v1",
		Objects: []RecordingPackageObject{
			{Path: "master.m3u8", SizeBytes: 100, SHA256: strings.Repeat("a", 64)},
			{Path: "720p/index.m3u8", SizeBytes: 100, SHA256: strings.Repeat("b", 64)},
			{Path: "720p/init.mp4", SizeBytes: 100, SHA256: strings.Repeat("c", 64)},
			{Path: "720p/seg-000000.m4s", SizeBytes: 100, SHA256: strings.Repeat("d", 64)},
		},
		Renditions: []RecordingRendition{{Name: "720p", Width: 1280, Height: 720, FrameRate: 30, VideoBitrate: 1_500_000, AudioBitrate: 128_000, DurationSeconds: 5, SegmentCount: 1}},
	}
}

func TestRecordingInventoryRejectsUnsafeAndMissingObjects(t *testing.T) {
	for _, name := range []string{"../secret", "/master.m3u8", "720p/%2e%2e/secret", "720p\\init.mp4", "https://evil/init.mp4", "720p/seg-000001.m4s?token=x", "package.json"} {
		t.Run(name, func(t *testing.T) {
			inv := packageFixture()
			inv.Objects[3].Path = name
			if _, err := ValidateRecordingInventory(inv); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("unsafe inventory accepted: %v", err)
			}
		})
	}
	inv := packageFixture()
	if _, err := ValidateRecordingInventory(inv); err != nil {
		t.Fatal(err)
	}
	inv.Objects = inv.Objects[:3]
	if _, err := ValidateRecordingInventory(inv); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing segment accepted: %v", err)
	}
	inv = packageFixture()
	inv.Objects = append(inv.Objects, inv.Objects[0])
	if _, err := ValidateRecordingInventory(inv); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate accepted: %v", err)
	}
}

func TestRecordingInventoryDigestBindsMetadataAndIgnoresOrdering(t *testing.T) {
	inv := packageFixture()
	a, err := RecordingInventoryDigest(inv)
	if err != nil {
		t.Fatal(err)
	}
	// Independently reproduced with Node crypto + JSON.stringify of the wire
	// fixture, so other clients cannot silently use a different digest format.
	if a != "b29d5b3efa97e926f585b0c62a480d45465730f16edd8059d4fb16792629e58b" {
		t.Fatalf("canonical wire digest changed: %s", a)
	}
	inv.Objects[0], inv.Objects[3] = inv.Objects[3], inv.Objects[0]
	b, err := RecordingInventoryDigest(inv)
	if err != nil || a != b {
		t.Fatalf("ordering changed digest: %s %s %v", a, b, err)
	}
	inv.InventoryDigest = a
	if _, err := ValidateRecordingInventory(inv); err != nil {
		t.Fatal(err)
	}
	inv.PresetVersion = "hls-v2"
	if _, err := ValidateRecordingInventory(inv); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("metadata mutation accepted: %v", err)
	}
}

func TestRecordingInventoryRejectsOversizeAndInvalidMetadata(t *testing.T) {
	for _, mutate := range []func(*RecordingPackageInventory){
		func(i *RecordingPackageInventory) { i.SchemaVersion = 2 },
		func(i *RecordingPackageInventory) { i.Objects[0].SizeBytes = 128<<20 + 1 },
		func(i *RecordingPackageInventory) { i.Objects[0].SHA256 = strings.Repeat("A", 64) },
		func(i *RecordingPackageInventory) { i.Renditions[0].FrameRate = 60 },
		func(i *RecordingPackageInventory) { i.Renditions[0].Width = 1279 },
		func(i *RecordingPackageInventory) { i.Renditions[0].DurationSeconds = 43201 },
		func(i *RecordingPackageInventory) { i.Renditions[0].SegmentCount = 2 },
		func(i *RecordingPackageInventory) { i.Renditions[0].SegmentCount = 0 },
		func(i *RecordingPackageInventory) { i.Renditions[0].SegmentCount = -1 },
		func(i *RecordingPackageInventory) { i.Renditions[0].SegmentCount = 1441 },
	} {
		inv := packageFixture()
		mutate(&inv)
		if _, err := ValidateRecordingInventory(inv); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid metadata accepted: %+v, %v", inv, err)
		}
	}
}

func TestRecordingSourceAndPackageCapacityAreIndependent(t *testing.T) {
	if err := ValidateRecordingSourceSize(50_000_000_000); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecordingSourceSize(50_000_000_001); !errors.Is(err, ErrRecordingSourceTooLarge) {
		t.Fatalf("source limit: %v", err)
	}
	if err := ValidateRecordingSourceSize(0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty source: %v", err)
	}
	estimate, err := EstimateRecordingPackageSize(9000, []int64{1_500_000, 3_000_000})
	if err != nil || estimate != 5_511_015_000 {
		t.Fatalf("2.5h estimate: %d %v", estimate, err)
	}
	_, err = EstimateRecordingPackageSize(18000, []int64{1_500_000, 3_000_000})
	if !errors.Is(err, ErrRecordingPackageEstimateTooLarge) {
		t.Fatalf("5h should fail preflight: %v", err)
	}
	encoded, _ := json.Marshal(packageFixture())
	var decoded RecordingPackageInventory
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateRecordingInventory(decoded); err != nil {
		t.Fatal(err)
	}
}

func TestRecordingPackageBoundaryRejectsCumulativeObjectBytes(t *testing.T) {
	inv := packageFixture()
	inv.Renditions[0].DurationSeconds = 2250
	inv.Renditions[0].SegmentCount = 75
	inv.Objects = inv.Objects[:3]
	for n := 0; n < 75; n++ {
		inv.Objects = append(inv.Objects, RecordingPackageObject{Path: fmt.Sprintf("720p/seg-%06d.m4s", n), SizeBytes: 128 << 20, SHA256: strings.Repeat("e", 64)})
	}
	if _, err := ValidateRecordingInventory(inv); !errors.Is(err, ErrRecordingPackageTooLarge) {
		t.Fatalf("cumulative size accepted: %v", err)
	}
	for _, duration := range []float64{math.NaN(), math.Inf(1), -1} {
		if _, err := EstimateRecordingPackageSize(duration, []int64{1_500_000}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid duration accepted: %v", err)
		}
	}
}

func TestDecodeRecordingInventoryRejectsUnknownAndTrailingData(t *testing.T) {
	inv := packageFixture()
	inv.InventoryDigest, _ = RecordingInventoryDigest(inv)
	data, _ := json.Marshal(inv)
	if _, err := DecodeRecordingInventory(strings.NewReader(string(data))); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{string(data) + "{}", `{"unexpected":true}`, strings.Repeat(" ", (8<<20)+1), "null"} {
		if _, err := DecodeRecordingInventory(strings.NewReader(payload)); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid payload accepted: %v", err)
		}
	}
	inv.InventoryDigest = ""
	data, _ = json.Marshal(inv)
	if _, err := DecodeRecordingInventory(strings.NewReader(string(data))); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsigned inventory accepted: %v", err)
	}
}
