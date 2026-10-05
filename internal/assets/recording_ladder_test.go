package assets

import (
	"strings"
	"testing"
)

func TestRecordingLadderAdds480WithoutBreakingLegacyPackages(t *testing.T) {
	inv := packageFixture()
	for _, r := range []RecordingRendition{
		{Name: "1080p", Width: 1920, Height: 1080, FrameRate: 30, VideoBitrate: 3000000, AudioBitrate: 128000, DurationSeconds: 5, SegmentCount: 1},
		{Name: "480p", Width: 854, Height: 480, FrameRate: 30, VideoBitrate: 800000, AudioBitrate: 128000, DurationSeconds: 5, SegmentCount: 1},
	} {
		inv.Renditions = append(inv.Renditions, r)
		for _, path := range []string{"index.m3u8", "init.mp4", "seg-000000.m4s"} {
			inv.Objects = append(inv.Objects, RecordingPackageObject{Path: r.Name + "/" + path, SizeBytes: 100, SHA256: strings.Repeat("a", 64)})
		}
		if _, err := ValidateRecordingInventory(inv); err != nil {
			t.Fatal(err)
		}
	}
	inv.Renditions[2].Height = 720
	if _, err := ValidateRecordingInventory(inv); err == nil {
		t.Fatal("accepted oversized 480p")
	}
	if _, err := EstimateRecordingPackageSize(9000, []int64{800000, 1500000, 3000000}); err != nil {
		t.Fatal(err)
	}
	if _, err := EstimateRecordingPackageSize(18000, []int64{800000, 1500000, 3000000}); err == nil {
		t.Fatal("accepted oversized package")
	}
}
