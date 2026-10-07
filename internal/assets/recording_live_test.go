package assets

import (
	"errors"
	"strings"
	"testing"
)

func liveBatch(start, end float64) map[string]RecordingLiveFragment {
	out := map[string]RecordingLiveFragment{}
	for _, name := range []string{"480p", "720p", "1080p"} {
		out[name] = RecordingLiveFragment{Start: start, End: end, Codecs: "avc1.64001f,mp4a.40.2"}
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
