package recordingprocessing

import (
	"errors"
	"strings"
	"testing"

	"hhc/asset-api/internal/assets"
)

const sourceProbeFixture = `{"streams":[{"codec_type":"video","width":1920,"height":1080,"sample_aspect_ratio":"1:1","avg_frame_rate":"30000/1001","color_transfer":"bt709"},{"codec_type":"audio"}],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"9000"}}`

func TestBrowserSourcePlanMatchesCLIContract(t *testing.T) {
	plan, err := PlanBrowserSource([]byte(sourceProbeFixture))
	if err != nil || len(plan.Renditions) != 2 || plan.Renditions[0].VideoBitrate != 1500000 || plan.Renditions[1].VideoBitrate != 3000000 || plan.Renditions[0].AudioBitrate != 128000 || plan.Renditions[0].SegmentCount != 300 || plan.Renditions[1].Width != 1920 || plan.Renditions[0].FrameRate != 30000.0/1001 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	want, err := assets.EstimateRecordingPackageSize(9000, []int64{1500000, 3000000})
	if err != nil || plan.EstimatedBytes != want {
		t.Fatal("estimate drift")
	}
	small := strings.ReplaceAll(strings.ReplaceAll(sourceProbeFixture, `"width":1920`, `"width":640`), `"height":1080`, `"height":360`)
	plan, err = PlanBrowserSource([]byte(small))
	if err != nil || len(plan.Renditions) != 1 || plan.Renditions[0].Width != 640 || plan.Renditions[0].Height != 360 {
		t.Fatalf("upscaled source %+v %v", plan, err)
	}
	if _, err := PlanBrowserSource([]byte(strings.ReplaceAll(sourceProbeFixture, `"9000"`, `"18000"`))); !errors.Is(err, assets.ErrRecordingPackageEstimateTooLarge) {
		t.Fatalf("oversized estimate %v", err)
	}
	for _, bad := range []string{
		strings.ReplaceAll(sourceProbeFixture, "bt709", "smpte2084"),
		strings.ReplaceAll(sourceProbeFixture, `"codec_type":"audio"`, `"codec_type":"data"`),
		strings.ReplaceAll(sourceProbeFixture, `"9000"`, `"NaN"`),
		strings.ReplaceAll(sourceProbeFixture, `"width":1920`, `"width":99999`),
		strings.ReplaceAll(sourceProbeFixture, `"color_transfer":"bt709"`, `"color_transfer":"bt709","side_data_list":[{"rotation":90}]`),
		sourceProbeFixture + `{}`,
	} {
		if _, err := PlanBrowserSource([]byte(bad)); err == nil {
			t.Fatal("unsupported source accepted")
		}
	}
}
