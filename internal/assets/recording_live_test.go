package assets

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func liveBatch(start, end float64) map[string]RecordingLiveFragment {
	out := map[string]RecordingLiveFragment{}
	for _, name := range []string{"480p", "720p", "1080p"} {
		out[name] = RecordingLiveFragment{Start: start, End: end, FrameRate: 30, Codecs: "avc1.64001f,mp4a.40.2"}
	}
	return out
}
func TestLiveProgressWaitsForAllRenditions(t *testing.T) {
	batch := liveBatch(0, 30)
	delete(batch, "480p")
	if _, err := AppendLiveSegment(nil, batch, false); !errors.Is(err, ErrCaptureMissingObjects) {
		t.Fatalf("missing rendition: %v", err)
	}
	first, err := AppendLiveSegment(nil, liveBatch(0, 30), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = AppendLiveSegment(first, liveBatch(31, 61), false); !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("gap: %v", err)
	}
	if _, err = AppendLiveSegment(first, liveBatch(30, 31), false); !errors.Is(err, ErrCaptureMissingObjects) {
		t.Fatalf("unsealed short tail: %v", err)
	}
	if _, err = AppendLiveSegment(first, liveBatch(30, 31), true); err != nil {
		t.Fatal(err)
	}
}
func TestLiveEventPlaylistAppendOnly(t *testing.T) {
	var segments []RecordingLiveSegment
	var previous string
	for i := 0; i < 1440; i++ {
		var err error
		segments, err = AppendLiveSegment(segments, liveBatch(float64(i*30), float64((i+1)*30)), false)
		if err != nil {
			t.Fatal(err)
		}
		if i != 179 && i != 180 && i != 1439 {
			continue
		}
		playlists, err := RecordingLivePlaylists(segments, false)
		if err != nil {
			t.Fatal(err)
		}
		current := string(playlists["720p/index.m3u8"])
		if !strings.HasPrefix(current, previous) || !strings.Contains(current, "#EXT-X-MEDIA-SEQUENCE:0\n") || !strings.Contains(current, "#EXT-X-PLAYLIST-TYPE:EVENT\n") || !strings.Contains(current, "seg-000000.m4s") || strings.Contains(current, "ENDLIST") {
			t.Fatal("EVENT changed history")
		}
		if int64(len(current)) > RecordingPlaylistMaxBytes {
			t.Fatal("12h playlist exceeds limit")
		}
		previous = current
	}
	final, err := RecordingLivePlaylists(segments, true)
	if err != nil {
		t.Fatal(err)
	}
	if string(final["720p/index.m3u8"]) != previous+"#EXT-X-ENDLIST\n" {
		t.Fatal("normal end changed historical playlist")
	}
}

func TestLiveFrameRateStableAndMasterMeasured(t *testing.T) {
	for _, rate := range []float64{30, 30000.0 / 1001} {
		batch := liveBatch(0, 30)
		for name, fragment := range batch {
			fragment.FrameRate = rate
			batch[name] = fragment
		}
		history, err := AppendLiveSegment(nil, batch, false)
		if err != nil {
			t.Fatal(err)
		}
		playlists, err := RecordingLivePlaylists(history, false)
		expected := "FRAME-RATE=30.000"
		if rate != 30 {
			expected = "FRAME-RATE=29.970"
		}
		if err != nil || strings.Count(string(playlists["master.m3u8"]), expected) != 3 {
			t.Fatalf("measured master: %v %s", err, playlists["master.m3u8"])
		}
		mixed := liveBatch(30, 60)
		for name, fragment := range mixed {
			fragment.FrameRate = rate
			mixed[name] = fragment
		}
		fragment := mixed["480p"]
		if rate == 30 {
			fragment.FrameRate = 30000.0 / 1001
		} else {
			fragment.FrameRate = 30
		}
		mixed["480p"] = fragment
		if _, err := AppendLiveSegment(nil, mixed, false); !errors.Is(err, ErrInvalidUpload) {
			t.Fatalf("mixed fps: %v", err)
		}
		changed := liveBatch(30, 60)
		for name, f := range changed {
			f.FrameRate = fragment.FrameRate
			changed[name] = f
		}
		if _, err := AppendLiveSegment(history, changed, false); !errors.Is(err, ErrInvalidUpload) {
			t.Fatalf("changed fps: %v", err)
		}
	}
	for _, rate := range []float64{0, 29.97, 25, 60, math.NaN(), math.Inf(1)} {
		batch := liveBatch(0, 30)
		for name, f := range batch {
			f.FrameRate = rate
			batch[name] = f
		}
		if _, err := AppendLiveSegment(nil, batch, false); !errors.Is(err, ErrInvalidUpload) {
			t.Fatalf("unknown rate %v: %v", rate, err)
		}
	}
}

func TestLiveLegacyHistoryIsPreviouslyVerified30FPS(t *testing.T) {
	var history []RecordingLiveSegment
	if err := json.Unmarshal([]byte(`[{"sequence":0,"renditions":{"1080p":{"start":0,"end":30,"codecs":"avc1.64001f,mp4a.40.2"},"720p":{"start":0,"end":30,"codecs":"avc1.64001f,mp4a.40.2"},"480p":{"start":0,"end":30,"codecs":"avc1.64001f,mp4a.40.2"}}}]`), &history); err != nil {
		t.Fatal(err)
	}
	playlists, err := RecordingLivePlaylists(history, false)
	if err != nil || strings.Count(string(playlists["master.m3u8"]), "FRAME-RATE=30.000") != 3 {
		t.Fatalf("legacy master: %v", err)
	}
	if _, err := AppendLiveSegment(history, liveBatch(30, 60), false); err != nil {
		t.Fatal(err)
	}
	batch := liveBatch(30, 60)
	for name, f := range batch {
		f.FrameRate = 30000.0 / 1001
		batch[name] = f
	}
	if _, err := AppendLiveSegment(history, batch, false); !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("legacy rate change: %v", err)
	}
}
