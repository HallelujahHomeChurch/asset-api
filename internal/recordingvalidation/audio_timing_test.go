package recordingvalidation

import (
	"encoding/json"
	"math"
	"testing"
)

func audioTimingJSON(pts []string, source bool) []byte {
	stream := map[string]any{"codec_name": "aac", "codec_type": "audio", "profile": "LC", "sample_rate": "48000"}
	if source {
		stream["id"] = "0x2"
		stream["time_base"] = "1/48000"
		stream["channels"] = 2
	}
	packets := make([]map[string]string, len(pts))
	for i, p := range pts {
		packets[i] = map[string]string{"pts_time": p, "duration_time": "0.021333"}
	}
	b, _ := json.Marshal(map[string]any{"streams": []any{stream}, "packets": packets})
	return b
}

func TestAudioRemuxRequiresExactDeclaredSourceTimeline(t *testing.T) {
	pts := []string{"0.000000", "0.021333", "0.098667"}
	for _, tail := range []uint32{1024, 3712} {
		source, err := sourceAudioTimeline(audioTimingJSON(pts, true), map[uint32][]uint32{2: {1024, 3712, tail}})
		if err != nil {
			t.Fatal(err)
		}
		start, end, err := audioBoundsForSource(audioTimingJSON(pts, false), source, false)
		if err != nil || start != 0 || math.Abs(end-(.098667+float64(tail)/48000)) > .000001 {
			t.Fatalf("declared gap/tail rejected: %f %f %v", start, end, err)
		}
		for name, bad := range map[string][]string{
			"missing":      {"0.000000", "0.098667"},
			"extra packet": {"0.000000", "0.021333", "0.098667", "0.120000"},
			"reordered":    {"0.000000", "0.098667", "0.021333"},
			"extra gap":    {"0.000000", "0.021333", "0.099667"},
			"overlap":      {"0.000000", "0.021333", "0.097667"},
			"filled gap":   {"0.000000", "0.021333", "0.042667"},
		} {
			t.Run(name, func(t *testing.T) {
				if _, _, err := audioBoundsForSource(audioTimingJSON(bad, false), source, false); err == nil {
					t.Fatal("accepted changed output timeline")
				}
			})
		}
	}
	for name, durations := range map[string]map[uint32][]uint32{
		"wrong track":    {3: {1024, 3712, 1024}},
		"missing":        {2: {1024, 3712}},
		"zero":           {2: {1024, 0, 1024}},
		"undeclared gap": {2: {1024, 1024, 1024}},
		"overlap":        {2: {1024, 4000, 1024}},
		"unbounded":      {2: {1024, 48001, 1024}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := sourceAudioTimeline(audioTimingJSON(pts, true), durations); err == nil {
				t.Fatal("accepted invalid source")
			}
		})
	}
	for _, field := range []string{"codec_name", "profile", "sample_rate", "time_base", "id", "channels"} {
		t.Run(field, func(t *testing.T) {
			var p map[string]any
			json.Unmarshal(audioTimingJSON(pts, true), &p)
			delete(p["streams"].([]any)[0].(map[string]any), field)
			data, _ := json.Marshal(p)
			if _, err := sourceAudioTimeline(data, map[uint32][]uint32{2: {1024, 3712, 1024}}); err == nil {
				t.Fatal("accepted invalid stream")
			}
		})
	}
}

func TestAudioRemuxRejectsOutputOverlappingDeclaredInterval(t *testing.T) {
	pts := []string{"0.000000", "0.010000"}
	source, err := sourceAudioTimeline(audioTimingJSON(pts, true), map[uint32][]uint32{2: {480, 1024}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := audioBoundsForSource(audioTimingJSON(pts, false), source, false); err == nil {
		t.Fatal("accepted nominal AAC frame overlapping declared interval")
	}
}

func TestAudioRemuxAllowsOnlyFinalRecordingPacketTrim(t *testing.T) {
	for _, finalSegment := range []bool{false, true} {
		for _, durations := range [][]uint32{{480, 1024}, {1024, 480}} {
			pts := []string{"0.000000", "0.021333"}
			if durations[0] == 480 {
				pts[1] = "0.010000"
			}
			source, err := sourceAudioTimeline(audioTimingJSON(pts, true), map[uint32][]uint32{2: durations})
			if err != nil {
				t.Fatal(err)
			}
			_, end, err := audioBoundsForSource(audioTimingJSON(pts, false), source, finalSegment)
			wantTrim := finalSegment && durations[1] == 480
			if (err == nil) != wantTrim {
				t.Fatalf("final=%v durations=%v err=%v", finalSegment, durations, err)
			}
			if wantTrim && math.Abs(end-.031333) > .000001 {
				t.Fatalf("lost declared end trim: %f", end)
			}
		}
	}
}
