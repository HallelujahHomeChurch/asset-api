package r2

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLivePointerUsesConditionalMonotonicWrite(t *testing.T) {
	id := strings.Repeat("a", 32)
	current := `{"revision":2,"lastSequence":1}`
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("ETag", `"old"`)
			io.WriteString(w, current)
			return
		}
		if r.Method != "PUT" {
			t.Errorf("method %s", r.Method)
			w.WriteHeader(500)
			return
		}
		puts++
		if r.Header.Get("If-Match") != `"old"` {
			t.Errorf("missing CAS: %v", r.Header)
		}
		b, _ := io.ReadAll(r.Body)
		current = string(b)
		w.Header().Set("ETag", `"new"`)
	}))
	defer server.Close()
	store := packageListStore(server)
	if err := store.AdvanceLivePointer(context.Background(), id, 3, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceLivePointer(context.Background(), id, 2, 1); err != nil {
		t.Fatal(err)
	}
	if puts != 1 || !strings.Contains(current, `"revision":3`) {
		t.Fatalf("regressed pointer: %s %d", current, puts)
	}
}
func TestLiveStorageCannotCopyAcrossCapturesOrSignFinal(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	store := packageListStore(server)
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	if err := store.PublishLiveObject(context.Background(), "recordings/packages/"+a+"/final/attempt/720p/init.mp4", "recordings/captures/"+b+"/final/720p/init.mp4", "etag"); err == nil {
		t.Fatal("cross capture copy")
	}
	if err := store.PutLivePlaylist(context.Background(), a, 1, "../bad", []byte("#EXTM3U\n")); err == nil {
		t.Fatal("unbounded playlist key")
	}
}

func TestLivePointerRejectsConflictingRevisionAndTrailingJSON(t *testing.T) {
	for _, raw := range []string{`{"revision":3,"lastSequence":2}`, `{"revision":3,"lastSequence":2} {}`} {
		t.Run(raw, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Fatal("unexpected write")
				}
				w.Header().Set("ETag", `"old"`)
				io.WriteString(w, raw)
			}))
			defer server.Close()
			if err := packageListStore(server).AdvanceLivePointer(context.Background(), strings.Repeat("a", 32), 3, 1); err == nil {
				t.Fatal("conflicting pointer accepted")
			}
		})
	}
}

func TestLivePointerCreationRecoversLostConditionalRace(t *testing.T) {
	current := ""
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			if current == "" {
				w.WriteHeader(404)
				io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
				return
			}
			w.Header().Set("ETag", `"winner"`)
			io.WriteString(w, current)
			return
		}
		puts++
		if r.Header.Get("If-None-Match") != "*" {
			t.Error("missing create fence")
		}
		current = `{"revision":1,"lastSequence":0}`
		w.WriteHeader(412)
		io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
	}))
	defer server.Close()
	if err := packageListStore(server).AdvanceLivePointer(context.Background(), strings.Repeat("a", 32), 1, 0); err != nil {
		t.Fatal(err)
	}
	if puts != 1 {
		t.Fatalf("duplicate mutable write: %d", puts)
	}
}
func TestLivePlaylistReplayRequiresIdenticalBytes(t *testing.T) {
	existing := "#EXTM3U\nold\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			if r.Header.Get("If-None-Match") != "*" {
				t.Error("missing immutable fence")
			}
			w.WriteHeader(412)
			io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
			return
		}
		io.WriteString(w, existing)
	}))
	defer server.Close()
	store := packageListStore(server)
	id := strings.Repeat("a", 32)
	if err := store.PutLivePlaylist(context.Background(), id, 1, "master.m3u8", []byte(existing)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutLivePlaylist(context.Background(), id, 1, "master.m3u8", []byte("#EXTM3U\nnew\n")); err != ErrLiveConflict {
		t.Fatalf("changed revision: %v", err)
	}
}
