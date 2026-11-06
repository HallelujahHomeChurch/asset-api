package recordingvalidation

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
)

// Controlled provider latency isolates the storage-work change; this is not a production SLO.
func BenchmarkLivePublicationBeforeAfter(b *testing.B) {
	probe, p, memory, declarations, mediaBytes := liveMovingMediaFixture(b, 300)
	first, err := probe.ValidateLiveSegment(context.Background(), p.ID, "warm", 0, declarations)
	if err != nil {
		b.Fatal(err)
	}
	evidence := make(map[string]assets.RecordingLiveValidation)
	for name, fragment := range first {
		receipt := *fragment.Validation
		receipt.InitETag = "etag"
		evidence[name] = receipt
		memory.bytes["recordings/captures/"+p.ID+"/final/"+receipt.Init.Path] = bytes.Clone(memory.bytes[p.StagingKey(receipt.Init.Path)])
	}
	for _, latency := range []time.Duration{50 * time.Millisecond, 250 * time.Millisecond} {
		for _, reuse := range []bool{false, true} {
			b.Run(fmt.Sprintf("provider_%dms/reuse_%t", latency.Milliseconds(), reuse), func(b *testing.B) {
				objects := latencyPublicationObjects{latencyLiveObjects{memory, latency}}
				probe := probe
				probe.Objects = objects
				if reuse {
					probe.LiveInitEvidence = evidence
				}
				b.SetBytes(mediaBytes)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					attempt := fmt.Sprintf("bench-%d-%t-%d", latency.Milliseconds(), reuse, i)
					batch, err := probe.ValidateLiveSegment(context.Background(), p.ID, attempt, 0, declarations)
					if err != nil {
						b.Fatal(err)
					}
					history, err := assets.AppendLiveSegment(nil, batch, false, 300)
					if err != nil {
						b.Fatal(err)
					}
					claim := postgres.RecordingLiveClaim{Capture: assets.RecordingCapture{ID: p.ID}, ClaimID: attempt, Objects: declarations}
					if _, err := publishLiveObjects(context.Background(), objects, claim, batch); err != nil {
						b.Fatal(err)
					}
					playlists, err := assets.RecordingLivePlaylists(history, false, 300)
					if err != nil {
						b.Fatal(err)
					}
					if reuse {
						if err := publishLivePlaylists(context.Background(), objects, p.ID, 1, playlists); err != nil {
							b.Fatal(err)
						}
					} else {
						for name, data := range playlists {
							if err := objects.PutLivePlaylist(context.Background(), p.ID, 1, name, data); err != nil {
								b.Fatal(err)
							}
						}
					}
					if err := objects.AdvanceLivePointer(context.Background(), p.ID, 1, 0); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				b.ReportMetric(10.01*float64(b.N)/b.Elapsed().Seconds(), "media_s/wall_s")
			})
		}
	}
}

type latencyPublicationObjects struct{ latencyLiveObjects }

func (s latencyPublicationObjects) PublishLiveObject(ctx context.Context, from, to, etag string) (string, error) {
	if err := s.pause(ctx); err != nil {
		return "", err
	}
	return "etag", s.PackageObjects.CopyPackageObject(ctx, from, to, etag)
}
func (s latencyPublicationObjects) PutLivePlaylist(ctx context.Context, _ string, _ int64, _ string, _ []byte) error {
	return s.pause(ctx)
}
func (s latencyPublicationObjects) AdvanceLivePointer(ctx context.Context, _ string, _ int64, _ int) error {
	if err := s.pause(ctx); err != nil {
		return err
	}
	return s.pause(ctx)
}
