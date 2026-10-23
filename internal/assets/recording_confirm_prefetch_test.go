package assets

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type confirmHookObjects struct {
	*packageObjects
	list func() (map[string]int64, error)
}

func (o confirmHookObjects) ListPackageObjects(context.Context, string, int) (map[string]int64, error) {
	return o.list()
}

func TestConfirmRevalidatesAfterStorageWait(t *testing.T) {
	for _, change := range []string{"owner", "state", "expiry", "declaration", "failed", "missing", "storage"} {
		t.Run(change, func(t *testing.T) {
			s, repo, objects, now, c := captureTest(t)
			object := RecordingPackageObject{Path: "720p/seg-000000.m4s", SizeBytes: 10, SHA256: strings.Repeat("a", 64)}
			declareCapture(t, s, c, []RecordingPackageObject{object})
			storageErr := errors.New("synthetic storage unavailable")
			s.objects = confirmHookObjects{objects, func() (map[string]int64, error) {
				current := repo.captures[c.ID]
				switch change {
				case "owner":
					current.ActorID = "another-actor"
				case "state":
					current.State = "freezing"
				case "expiry":
					*now = now.Add(RecordingUploadTTL)
				case "declaration":
					current.Objects[0].SHA256 = strings.Repeat("b", 64)
				case "failed":
					current.Objects[0].State = "failed"
				}
				repo.captures[c.ID] = current
				if change == "storage" {
					return nil, storageErr
				}
				if change == "missing" {
					return map[string]int64{}, nil
				}
				return map[string]int64{object.Path: 10}, nil
			}}
			_, err := s.Confirm(context.Background(), c.ID, c.ActorID, "confirm", []string{object.Path})
			want := ErrConflict
			switch change {
			case "owner":
				want = ErrForbidden
			case "expiry":
				want = ErrCaptureExpired
			case "storage":
				want = storageErr
			case "missing":
				want = ErrCaptureMissingObjects
			}
			if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
			current := repo.captures[c.ID]
			if _, ok := current.Receipts["confirm"]; ok || current.LastMediaConfirmedAt != nil {
				t.Fatal("rejected evidence persisted")
			}
		})
	}
}

func TestConfirmReplayAvoidsStorageAndPreservesWatermark(t *testing.T) {
	s, repo, objects, now, c := captureTest(t)
	object := RecordingPackageObject{Path: "720p/seg-000000.m4s", SizeBytes: 10, SHA256: strings.Repeat("a", 64)}
	declareCapture(t, s, c, []RecordingPackageObject{object})
	objects.sizes["recordings/packages/"+c.ID+"/staging/"+object.Path] = 10
	first, err := s.Confirm(context.Background(), c.ID, c.ActorID, "confirm", []string{object.Path})
	if err != nil {
		t.Fatal(err)
	}
	watermark := *repo.captures[c.ID].LastMediaConfirmedAt
	*now = now.Add(time.Minute)
	if _, err = s.Confirm(context.Background(), c.ID, c.ActorID, "duplicate", []string{object.Path}); err != nil {
		t.Fatal(err)
	}
	if !repo.captures[c.ID].LastMediaConfirmedAt.Equal(watermark) {
		t.Fatal("duplicate advanced watermark")
	}
	current := repo.captures[c.ID]
	current.State = "expired"
	repo.captures[c.ID] = current
	s.objects = confirmHookObjects{objects, func() (map[string]int64, error) { t.Fatal("replay accessed storage"); return nil, nil }}
	replay, err := s.Confirm(context.Background(), c.ID, c.ActorID, "confirm", []string{object.Path})
	if err != nil || replay.Receipt != first.Receipt {
		t.Fatalf("terminal replay: %+v %v", replay, err)
	}
}

func TestConcurrentConfirmReceiptWinsOverStorageFailure(t *testing.T) {
	s, repo, objects, _, c := captureTest(t)
	object := RecordingPackageObject{Path: "720p/seg-000000.m4s", SizeBytes: 10, SHA256: strings.Repeat("a", 64)}
	declareCapture(t, s, c, []RecordingPackageObject{object})
	objects.sizes["recordings/packages/"+c.ID+"/staging/"+object.Path] = 10
	var accepted RecordingCaptureResult
	s.objects = confirmHookObjects{objects, func() (map[string]int64, error) {
		other := NewRecordingCaptureService(repo, objects, s.now)
		var err error
		accepted, err = other.Confirm(context.Background(), c.ID, c.ActorID, "confirm", []string{object.Path})
		if err != nil {
			t.Fatal(err)
		}
		return nil, errors.New("synthetic storage failure after concurrent commit")
	}}
	result, err := s.Confirm(context.Background(), c.ID, c.ActorID, "confirm", []string{object.Path})
	if err != nil || result.Receipt != accepted.Receipt {
		t.Fatalf("concurrent receipt: %+v %v", result, err)
	}
}
