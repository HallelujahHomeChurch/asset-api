package recordingvalidation

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"hhc/asset-api/internal/assets"
)

func timingWords(values ...uint32) []byte {
	var data []byte
	for _, value := range values {
		data = binary.BigEndian.AppendUint32(data, value)
	}
	return data
}

func timingBox(kind string, children ...[]byte) []byte {
	data := append(make([]byte, 4), kind...)
	for _, child := range children {
		data = append(data, child...)
	}
	binary.BigEndian.PutUint32(data[:4], uint32(len(data)))
	return data
}

func TestFragmentSampleDurationsReadsExplicitAndDefaultTables(t *testing.T) {
	for _, tc := range []struct {
		name        string
		header, run []byte
		want        []uint32
	}{
		{"explicit", timingWords(8, 2, 1024), timingWords(0x100, 3, 1024, 3712, 1024), []uint32{1024, 3712, 1024}},
		{"tfhd default", timingWords(8, 2, 960), timingWords(0, 2), []uint32{960, 960}},
		{"trex default", timingWords(0, 2), timingWords(0, 2), []uint32{1024, 1024}},
		{"optional fields", timingWords(0x3b, 2, 0, 0, 1, 1024, 100, 0), timingWords(0x1000f05, 1, 0, 0, 3712, 100, 0, 0), []uint32{3712}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := timingBox("moov", timingBox("mvex", timingBox("trex", timingWords(0, 2, 1, 1024, 0, 0))))
			data = append(data, timingBox("moof", timingBox("traf", timingBox("tfhd", tc.header), timingBox("trun", tc.run)))...)
			path := filepath.Join(t.TempDir(), "fragment.mp4")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := fragmentSampleDurations(path)
			if err != nil || !reflect.DeepEqual(got[2], tc.want) {
				t.Fatalf("durations=%v error=%v", got, err)
			}
		})
	}
}

func TestFragmentSampleDurationsRejectsMalformedMetadata(t *testing.T) {
	validHeader := timingBox("tfhd", timingWords(8, 2, 1024))
	for name, data := range map[string][]byte{
		"truncated box":            {0, 0, 0, 20, 'm', 'o', 'o', 'f'},
		"overflow size":            append([]byte{0, 0, 0, 1, 'm', 'o', 'o', 'f'}, timingWords(0xffffffff, 0xffffffff)...),
		"truncated sample":         timingBox("moof", timingBox("traf", validHeader, timingBox("trun", timingWords(0x100, 2, 1024)))),
		"zero duration":            timingBox("moof", timingBox("traf", validHeader, timingBox("trun", timingWords(0x100, 1, 0)))),
		"too many samples":         timingBox("moof", timingBox("traf", validHeader, timingBox("trun", timingWords(0, 4097)))),
		"missing track":            timingBox("moof", timingBox("traf", timingBox("trun", timingWords(0, 1)))),
		"duplicate header":         timingBox("moof", timingBox("traf", validHeader, validHeader, timingBox("trun", timingWords(0, 1)))),
		"unknown flags":            timingBox("moof", timingBox("traf", validHeader, timingBox("trun", timingWords(0x10, 1)))),
		"truncated optional field": timingBox("moof", timingBox("traf", validHeader, timingBox("trun", timingWords(1, 1)))),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fragment.mp4")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := fragmentSampleDurations(path); !errors.Is(err, assets.ErrInvalidUpload) {
				t.Fatalf("malformed metadata accepted: %v", err)
			}
		})
	}
}

func TestFragmentSampleDurationsPreservesRunOrderAndAggregateLimit(t *testing.T) {
	header := timingBox("tfhd", timingWords(8, 2, 1024))
	data := timingBox("moof", timingBox("traf", header, timingBox("trun", timingWords(0x100, 1, 3712)), timingBox("trun", timingWords(0, 1))))
	data = append(data, timingBox("moof", timingBox("traf", header, timingBox("trun", timingWords(0x100, 1, 960))))...)
	path := filepath.Join(t.TempDir(), "fragment.mp4")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := fragmentSampleDurations(path)
	if err != nil || !reflect.DeepEqual(got[2], []uint32{3712, 1024, 960}) {
		t.Fatalf("run order: %v %v", got, err)
	}
	data = timingBox("moof", timingBox("traf", header, timingBox("trun", timingWords(0, 4096)), timingBox("trun", timingWords(0, 1))))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fragmentSampleDurations(path); !errors.Is(err, assets.ErrInvalidUpload) {
		t.Fatalf("aggregate sample limit bypassed: %v", err)
	}
}

func TestDeclaredAACDurationStillRequiresExactPacketContinuityAndCount(t *testing.T) {
	r := assets.RecordingRendition{Width: 1280, Height: 720, FrameRate: 30}
	data := strings.Replace(segmentProbeFixture, `"sample_rate":"48000"`, `"sample_rate":"48000","id":"0x2","time_base":"1/48000"`, 1)
	data = strings.Replace(data, `"pts_time":"0.042666"`, `"pts_time":"0.098667"`, 1)
	if _, err := validatePackageSegmentProbe([]byte(data), r, nil); !errors.Is(err, assets.ErrInvalidUpload) {
		t.Fatalf("old probe should reject discontinuity: %v", err)
	}
	durations := map[uint32][]uint32{2: {1024, 3712, 1024}}
	if _, err := validatePackageSegmentProbe([]byte(data), r, durations); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"real gap":      strings.Replace(data, `"pts_time":"0.098667"`, `"pts_time":"0.099667"`, 1),
		"wrong track":   strings.Replace(data, `"id":"0x2"`, `"id":"0x3"`, 1),
		"bad time base": strings.Replace(data, `"1/48000"`, `"1/0"`, 1),
	} {
		if _, err := validatePackageSegmentProbe([]byte(bad), r, durations); !errors.Is(err, assets.ErrInvalidUpload) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	for _, bad := range [][]uint32{{1024, 3712}, {1024, 3712, 1024, 1024}, {1024, 0, 1024}} {
		if _, err := validatePackageSegmentProbe([]byte(data), r, map[uint32][]uint32{2: bad}); !errors.Is(err, assets.ErrInvalidUpload) {
			t.Fatalf("bad counts/durations accepted: %v", bad)
		}
	}
}
