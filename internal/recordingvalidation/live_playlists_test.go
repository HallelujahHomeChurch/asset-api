package recordingvalidation

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type gatedPlaylists struct {
	LiveObjects
	started chan struct{}
	release chan struct{}
	failure error
}

func (s gatedPlaylists) PutLivePlaylist(context.Context, string, int64, string, []byte) error {
	s.started <- struct{}{}
	<-s.release
	return s.failure
}
func TestLivePlaylistsJoinBeforePublicationCanAdvance(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			failure := errors.New("playlist write failed")
			objects := gatedPlaylists{started: make(chan struct{}, 4), release: make(chan struct{})}
			if fail {
				objects.failure = failure
			}
			playlists := map[string][]byte{"master.m3u8": {}, "1080p/index.m3u8": {}, "720p/index.m3u8": {}, "480p/index.m3u8": {}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- publishLivePlaylists(ctx, objects, "capture", 1, playlists) }()
			for i := 0; i < 4; i++ {
				select {
				case <-objects.started:
				case <-ctx.Done():
					close(objects.release)
					t.Fatal("playlist writes serialized")
				}
			}
			select {
			case <-done:
				close(objects.release)
				t.Fatal("returned before all playlists completed")
			default:
			}
			close(objects.release)
			err := <-done
			if fail && !errors.Is(err, failure) || !fail && err != nil {
				t.Fatal(err)
			}
		})
	}
}
