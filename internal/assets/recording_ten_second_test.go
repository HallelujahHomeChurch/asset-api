package assets

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestTenSecondProfileIsExplicitAndStable(t *testing.T) {
	batch := liveBatch(0, 10.01)
	for name, f := range batch {
		f.FrameRate = 30000.0 / 1001
		batch[name] = f
	}
	if _, err := AppendLiveSegment(nil, batch, false); err == nil {
		t.Fatal("legacy capture accepted short interior")
	}
	history, err := AppendLiveSegment(nil, batch, false, 300)
	if err != nil {
		t.Fatal(err)
	}
	playlists, err := RecordingLivePlaylists(history, false, 300)
	if err != nil || !strings.Contains(string(playlists["720p/index.m3u8"]), "#EXT-X-TARGETDURATION:11\n") {
		t.Fatalf("short playlist: %v", err)
	}
	for name, f := range batch {
		f.Start = 10.01
		f.End = 40.04
		batch[name] = f
	}
	if _, err := AppendLiveSegment(history, batch, false, 300); err == nil {
		t.Fatal("mixed profile accepted")
	}
	for name, f := range batch {
		f.End = 15
		batch[name] = f
	}
	if _, err := AppendLiveSegment(history, batch, false, 300); err == nil {
		t.Fatal("short interior accepted")
	}
	if _, err := AppendLiveSegment(history, batch, true, 300); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureProfileCannotChangeOnRetry(t *testing.T) {
	service, repo, _, _, legacy := captureTest(t)
	ctx := context.Background()
	created, err := service.CreateBroadcast(ctx, legacy.RecordingID, legacy.ActorID, "ten-second", 1, 300)
	if err != nil {
		t.Fatal(err)
	}
	if repo.captures[created.Capture.ID].SegmentFrames != 300 {
		t.Fatal("profile not persisted")
	}
	if _, err = service.CreateBroadcast(ctx, legacy.RecordingID, legacy.ActorID, "ten-second", 1, 900); !errors.Is(err, ErrConflict) {
		t.Fatalf("profile changed: %v", err)
	}
	if _, err = service.CreateBroadcast(ctx, legacy.RecordingID, legacy.ActorID, "ten-second", 1, 300); err != nil {
		t.Fatal(err)
	}
}

func TestTenSecondTimelineKeepsTwelveHourLimit(t *testing.T) {
	var history []RecordingLiveSegment
	for i := 0; i < 4315; i++ {
		batch := liveBatch(float64(i)*10.01, float64(i+1)*10.01)
		for name, f := range batch {
			f.FrameRate = 30000.0 / 1001
			batch[name] = f
		}
		var err error
		history, err = AppendLiveSegment(history, batch, false, 300)
		if err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
	}
	if _, err := RecordingLivePlaylists(history, false, 300); err != nil {
		t.Fatal(err)
	}
}
