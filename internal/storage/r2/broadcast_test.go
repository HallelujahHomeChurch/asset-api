package r2

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestBroadcastAuthorityConditionalWritesNeverRestoreScope(t *testing.T) {
	var data []byte
	etag := `"one"`
	var puts atomic.Int64
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/"+broadcastPolicyKey(strings.Repeat("a", 32))) {
			t.Errorf("unexpected policy key: %s", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		switch r.Method {
		case "GET":
			if data == nil {
				w.WriteHeader(404)
				io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
				return
			}
			w.Header().Set("ETag", etag)
			w.Write(data)
		case "PUT":
			if data == nil && r.Header.Get("If-None-Match") != "*" || data != nil && r.Header.Get("If-Match") != etag {
				t.Error("unconditional authority replacement")
			}
			puts.Add(1)
			data, _ = io.ReadAll(r.Body)
			etag = `"two"`
			w.Header().Set("ETag", etag)
		default:
			w.WriteHeader(405)
		}
	}))
	defer server.Close()
	store := packageListStore(server)
	id := strings.Repeat("a", 32)
	ctx := context.Background()
	p := broadcastRange{MemberState: "blocked", RecordingID: "11111111-1111-4111-8111-111111111111", Epoch: 1, RangeRevision: 1}
	write := func(p broadcastRange) error { b, _ := json.Marshal(p); return store.PutBroadcastRange(ctx, id, b) }
	if err := write(p); err != nil {
		t.Fatal(err)
	}
	start, end := 8, 20
	p.StartSequence = &start
	p.EndSequenceExclusive = &end
	p.RangeRevision = 2
	if err := write(p); err != nil {
		t.Fatal(err)
	}
	if err := write(p); err != nil || puts.Load() != 2 {
		t.Fatalf("replay rewrote authority: %d %v", puts.Load(), err)
	}
	stale := p
	stale.RangeRevision = 1
	if err := write(stale); err == nil {
		t.Fatal("stale writer restored policy")
	}
	expanded := p
	expanded.RangeRevision = 3
	e := 21
	expanded.EndSequenceExclusive = &e
	if err := write(expanded); err == nil {
		t.Fatal("frozen end expanded")
	}
	p.RangeRevision = 3
	p.Revoked = true
	if err := write(p); err != nil {
		t.Fatal(err)
	}
	unrevoked := p
	unrevoked.RangeRevision = 4
	unrevoked.Revoked = false
	if err := write(unrevoked); err == nil {
		t.Fatal("revocation removed")
	}
	raw, err := store.ReadBroadcastRange(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := parseBroadcastRange(raw)
	if err != nil || !stored.Revoked || puts.Load() != 3 {
		t.Fatalf("authority state: %+v writes %d %v", stored, puts.Load(), err)
	}
}
