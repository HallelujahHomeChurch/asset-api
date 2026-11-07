package telemetry

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRequestAccumulatorConcurrentAndClosed(t *testing.T) {
	ctx := observeRequest(context.Background(), httptest.NewRequest("GET", "/api/assets/public/synthetic", nil))
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() { defer wg.Done(); RecordPhase(ctx, "db_query_lifecycle", 100*time.Millisecond, false) }()
	}
	wg.Wait()
	o := ctx.Value(requestKey{}).(*requestObservation)
	values, failed, emit, _ := o.finish()
	if failed || !emit || len(values) != 1 || values[0].Count != 32 || values[0].TotalMS != 3200 {
		t.Fatal(values)
	}
	RecordPhase(ctx, "db_query_lifecycle", time.Second, true)
	_, _, again, _ := o.finish()
	if again {
		t.Fatal("summary repeated")
	}
}

type failingStorageBody struct{ closeCount int }

func (b *failingStorageBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (b *failingStorageBody) Close() error             { b.closeCount++; return io.ErrClosedPipe }
func TestStorageBodyPreservesFailuresAndRecordsOnce(t *testing.T) {
	for _, early := range []bool{false, true} {
		ctx := observeRequest(context.Background(), httptest.NewRequest("GET", "/", nil))
		raw := &failingStorageBody{}
		var source io.ReadCloser = raw
		if early {
			source = io.NopCloser(strings.NewReader("unread"))
		}
		body := StorageBody(ctx, source)
		if !early {
			_, err := body.Read(make([]byte, 8))
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal(err)
			}
		}
		err := body.Close()
		if !early && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal(err)
		}
		o := ctx.Value(requestKey{}).(*requestObservation)
		values, failed, _, reason := o.finish()
		if !failed || len(values) != 1 || values[0].Phase != "storage_body" || values[0].Count != 1 || reason == "" {
			t.Fatal(values, failed, reason)
		}
	}
}

func TestStorageEOFDoesNotHideCloseFailure(t *testing.T) {
	ctx := observeRequest(context.Background(), httptest.NewRequest("GET", "/", nil))
	body := StorageBody(ctx, eofCloseFailure{})
	io.ReadAll(body)
	if body.Close() == nil {
		t.Fatal("close error changed")
	}
	o := ctx.Value(requestKey{}).(*requestObservation)
	values, failed, _, reason := o.finish()
	if !failed || len(values) != 1 || values[0].Count != 1 || reason != "storage_body_error" {
		t.Fatal(values, failed, reason)
	}
}
