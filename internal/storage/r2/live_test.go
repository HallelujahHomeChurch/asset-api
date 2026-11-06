package r2

import (
	"context"
	"errors"
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
	if _, err := store.PublishLiveObject(context.Background(), "recordings/packages/"+a+"/final/attempt/720p/init.mp4", "recordings/captures/"+b+"/final/720p/init.mp4", "etag"); err == nil {
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

func TestLivePreviewPointerNeverRegressesOrTouchesMediaPointer(t *testing.T) {
	id := strings.Repeat("a", 32)
	current := `{"revision":2,"lastSequence":1}`
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/final/previews/current.json") {
			t.Fatal("touched media pointer", r.URL.Path)
		}
		w.Header().Set("ETag", `"etag"`)
		if r.Method == "GET" {
			io.WriteString(w, current)
			return
		}
		if r.Header.Get("If-Match") != `"etag"` {
			t.Error("missing conditional fence")
		}
		puts++
		data, _ := io.ReadAll(r.Body)
		current = string(data)
	}))
	defer server.Close()
	store := packageListStore(server)
	for _, last := range []int{5, 1, 5} {
		if err := store.AdvanceLivePreviewPointer(context.Background(), id, last); err != nil {
			t.Fatal(err)
		}
	}
	if puts != 1 || current != `{"revision":6,"lastSequence":5}` {
		t.Fatal(puts, current)
	}
	for _, last := range []int{-1, 4320} {
		if err := store.AdvanceLivePreviewPointer(context.Background(), id, last); err == nil {
			t.Fatal("bad boundary accepted")
		}
	}
}

func TestLivePublicationReturnsConditionalCopyIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Copy-Source-If-Match") != "source-version" {
			t.Error("missing source fence")
		}
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, `<CopyObjectResult><ETag>"stable-version"</ETag></CopyObjectResult>`)
	}))
	defer server.Close()
	id := strings.Repeat("a", 32)
	etag, err := packageListStore(server).PublishLiveObject(context.Background(), "recordings/packages/"+id+"/final/attempt/720p/init.mp4", "recordings/captures/"+id+"/final/720p/init.mp4", "source-version")
	if err != nil || etag != `"stable-version"` {
		t.Fatalf("published identity %q %v", etag, err)
	}
}

func TestArchiveCopyStaysWithinCaptureAndRequiresObjectVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Copy-Source-If-Match") != "verified-version" {
			t.Error("missing immutable source fence")
		}
		if r.Header.Get("X-Amz-Metadata-Directive") != "REPLACE" {
			t.Error("private metadata not reset")
		}
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, `<CopyObjectResult><ETag>"archive-version"</ETag></CopyObjectResult>`)
	}))
	defer server.Close()
	store := packageListStore(server)
	id := strings.Repeat("a", 32)
	source := "recordings/captures/" + id + "/final/720p/seg-000000.m4s"
	destination := "recordings/packages/" + id + "/final/attempt/720p/seg-000000.m4s"
	for _, tc := range []struct {
		source, destination, etag string
		ok                        bool
	}{
		{source, destination, "verified-version", true},
		{source, strings.Replace(destination, id, strings.Repeat("b", 32), 1), "verified-version", false},
		{source, strings.Replace(destination, "seg-000000", "seg-000001", 1), "verified-version", false},
		{source, destination, "", false},
		{source, destination, "invalid\r\nversion", false},
		{strings.Replace(source, "720p/seg-000000.m4s", "playlists/1/master.m3u8", 1), destination, "verified-version", false},
	} {
		if err := store.CopyPackageObject(context.Background(), tc.source, tc.destination, tc.etag); (err == nil) != tc.ok {
			t.Fatalf("copy scope accepted=%v err=%v", tc.ok, err)
		}
	}
}

func TestOpenNormalizesOnlyMissingObject(t *testing.T) {
	for _, tc := range []struct {
		code    string
		status  int
		missing bool
	}{{"NoSuchKey", 404, true}, {"NotFound", 404, true}, {"AccessDenied", 403, false}} {
		t.Run(tc.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(tc.status)
				io.WriteString(w, "<Error><Code>"+tc.code+"</Code></Error>")
			}))
			defer server.Close()
			_, err := packageListStore(server).Open(context.Background(), "recordings/captures/"+strings.Repeat("a", 32)+"/final/720p/init.mp4")
			if err == nil || errors.Is(err, ErrNotFound) != tc.missing {
				t.Fatalf("missing=%v err=%v", tc.missing, err)
			}
		})
	}
}
