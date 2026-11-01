package scanqueue

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"hhc/asset-api/internal/assets"
)

type budgetBlobs func(context.Context) (assets.BlobDownload, error)

func (f budgetBlobs) Open(ctx context.Context, _ string, _ assets.ByteRange, _ string) (assets.BlobDownload, error) {
	return f(ctx)
}

type budgetScanner func(context.Context, string) (string, error)

func (f budgetScanner) ScanFile(ctx context.Context, path string) (string, error) {
	return f(ctx, path)
}

type budgetRepository struct {
	*jobRepository
	apply func(context.Context) error
	retry func(context.Context, time.Time, time.Time) error
}

func (r *budgetRepository) ApplyScanResult(ctx context.Context, result assets.ScanResult, now time.Time) (bool, error) {
	if r.apply != nil {
		if err := r.apply(ctx); err != nil {
			return false, err
		}
	}
	return r.jobRepository.ApplyScanResult(ctx, result, now)
}
func (r *budgetRepository) ScheduleAssetScanRetry(ctx context.Context, id string, attempt int, details, category string, next, now time.Time) error {
	if r.retry != nil {
		if err := r.retry(ctx, next, now); err != nil {
			return err
		}
	}
	return r.jobRepository.ScheduleAssetScanRetry(ctx, id, attempt, details, category, next, now)
}

type budgetQueue struct {
	*jobQueue
	ack func(context.Context) error
}

func (q *budgetQueue) Ack(ctx context.Context, message Message) error {
	if q.ack != nil {
		if err := q.ack(ctx); err != nil {
			return err
		}
	}
	return q.jobQueue.Ack(ctx, message)
}

func TestScanAttemptBoundsPreparationDownloadAndScan(t *testing.T) {
	for _, phase := range []string{"prepare", "download", "scan", "late-prepare", "late-download", "late-scan"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				repo := &budgetRepository{jobRepository: &jobRepository{asset: queuedAsset()}}
				queue := &jobQueue{message: scanMessage(1)}
				scanned, retried := false, false
				scanner := budgetScanner(func(ctx context.Context, _ string) (string, error) {
					scanned = true
					if phase == "scan" {
						<-ctx.Done()
						return "", ctx.Err()
					}
					if phase == "late-scan" {
						time.Sleep(2 * time.Second)
					}
					return "", nil
				})
				var blobs AssetBlobs = jobBlobs{body: []byte("clean")}
				if phase == "download" || phase == "late-download" {
					blobs = budgetBlobs(func(ctx context.Context) (assets.BlobDownload, error) {
						if phase == "download" {
							<-ctx.Done()
							return assets.BlobDownload{}, ctx.Err()
						}
						time.Sleep(2 * time.Second)
						return (jobBlobs{body: []byte("clean")}).Open(ctx, "", assets.ByteRange{}, "")
					})
				}
				var workDeadline time.Time
				prepare := func(ctx context.Context) (FileScanner, string, error) {
					workDeadline, _ = ctx.Deadline()
					if phase == "prepare" {
						<-ctx.Done()
						return nil, "", ctx.Err()
					}
					if phase == "late-prepare" {
						time.Sleep(2 * time.Second)
					}
					return scanner, "sig-1", nil
				}
				repo.retry = func(ctx context.Context, next, now time.Time) error {
					retried = true
					if ctx.Err() != nil {
						t.Fatal("retry lost its reserved commit budget")
					}
					if now.Before(started.Add(time.Second)) || !next.After(now) {
						t.Fatal("retry delay starts before the failed work finishes")
					}
					deadline, _ := ctx.Deadline()
					if deadline.Sub(workDeadline) != scanCommitReserve {
						t.Fatal("commit reserve changed")
					}
					return nil
				}
				job := NewScanJob(repo, blobs, prepare, queue, 100, 5, time.Second)
				if done, err := job.RunOnce(context.Background()); err != nil || !done {
					t.Fatalf("run=%v %v", done, err)
				}
				if !retried || !queue.retried || queue.acked || repo.result.Status != "" {
					t.Fatalf("deadline produced result/ack: %+v %+v", repo.result, queue)
				}
				if scanned != (phase == "scan" || phase == "late-scan") {
					t.Fatalf("unexpected scan after %s", phase)
				}
				if elapsed := time.Since(started); elapsed < time.Second || elapsed > 2*time.Second {
					t.Fatalf("work budget not enforced: %s", elapsed)
				}
			})
		})
	}
}

func TestScanAttemptReservesCommitTimeWithoutExtendingLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		repo := &budgetRepository{jobRepository: &jobRepository{asset: queuedAsset()}}
		queue := &jobQueue{message: scanMessage(1)}
		var workDeadline time.Time
		prepare := func(ctx context.Context) (FileScanner, string, error) {
			workDeadline, _ = ctx.Deadline()
			return &jobScanner{}, "sig-current", nil
		}
		repo.apply = func(ctx context.Context) error {
			deadline, _ := ctx.Deadline()
			if deadline.Sub(workDeadline) != scanCommitReserve {
				t.Fatal("missing bounded commit reserve")
			}
			time.Sleep(2 * time.Second)
			return ctx.Err()
		}
		job := NewScanJob(repo, jobBlobs{body: []byte("clean")}, prepare, queue, 100, 5, time.Second)
		if _, err := job.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !queue.acked || repo.result.Status != assets.ScanClean || repo.result.Signature != "sig-current" || repo.result.ExpectedAttempt != repo.asset.ScanAttempts {
			t.Fatal("commit/fencing changed")
		}
		if time.Since(started) != 2*time.Second || job.lease != time.Second+scanCommitReserve {
			t.Fatal("lease extended")
		}
	})
}

func TestScanAttemptBoundsStalledCommitRetryAndAcknowledgement(t *testing.T) {
	for _, stage := range []string{"commit", "retry", "ack"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				repo := &budgetRepository{jobRepository: &jobRepository{asset: queuedAsset()}}
				queue := &budgetQueue{jobQueue: &jobQueue{message: scanMessage(1)}}
				stall := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
				prepare := fixedScanner(&jobScanner{})
				switch stage {
				case "commit":
					repo.apply = stall
				case "retry":
					prepare = func(context.Context) (FileScanner, string, error) {
						return nil, "", errors.New("signature unavailable")
					}
					repo.retry = func(ctx context.Context, _, _ time.Time) error { return stall(ctx) }
				case "ack":
					queue.ack = stall
				}
				job := NewScanJob(repo, jobBlobs{body: []byte("clean")}, prepare, queue, 100, 5, time.Second)
				if _, err := job.RunOnce(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("stalled %s=%v", stage, err)
				}
				if queue.acked || time.Since(started) != job.lease {
					t.Fatal("finalization exceeded lease or acknowledged after timeout")
				}
			})
		})
	}
}

func TestScanAttemptPreservesCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		repo := &budgetRepository{jobRepository: &jobRepository{asset: queuedAsset()}, retry: func(context.Context, time.Time, time.Time) error {
			t.Fatal("retry after caller cancellation")
			return nil
		}}
		queue := &jobQueue{message: scanMessage(1)}
		scanner := budgetScanner(func(ctx context.Context, _ string) (string, error) { <-ctx.Done(); return "", ctx.Err() })
		job := NewScanJob(repo, jobBlobs{body: []byte("clean")}, fixedScanner(scanner), queue, 100, 5, time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if _, err := job.RunOnce(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("caller deadline=%v", err)
		}
		if queue.acked || queue.retried || repo.result.Status != "" || time.Since(started) != 500*time.Millisecond {
			t.Fatal("caller deadline was detached or extended")
		}
	})
}
