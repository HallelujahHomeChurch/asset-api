package recordingprocessing

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"hhc/asset-api/internal/assets"
)

var (
	spoolPackageID     = regexp.MustCompile(`^[a-f0-9]{32}$`)
	spoolPath          = regexp.MustCompile(`^(480p|720p|1080p)/(index\.m3u8|init\.mp4|seg-[0-9]{6}\.m4s)$`)
	ErrScratchCapacity = errors.New("recording_scratch_capacity_exhausted")
	ErrOutputUpload    = errors.New("recording_output_upload_failed")
)

type RecordingOutputObjects interface {
	PutRecordingObject(context.Context, string, io.ReadSeeker, int64, string) error
}

// OutputSpool acknowledges an FFmpeg PUT only after one closed media object has
// reached R2. Native HTTP backpressure bounds scratch to a single <=128MiB file;
// deleting it never reduces the cumulative package-byte budget.
type OutputSpool struct {
	mu                              sync.Mutex
	objects                         RecordingOutputObjects
	packageID, dir, url, host, path string
	server                          *http.Server
	cancel                          context.CancelFunc
	done                            chan struct{}
	media                           map[string]assets.RecordingPackageObject
	playlists                       map[string][]byte
	bytes                           int64
	err                             error
}

func NewOutputSpool(ctx context.Context, objects RecordingOutputObjects, packageID, scratchRoot string) (*OutputSpool, error) {
	if objects == nil || !spoolPackageID.MatchString(packageID) {
		return nil, assets.ErrInvalidInput
	}
	dir, err := os.MkdirTemp(scratchRoot, "hhc-hls-spool-")
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &OutputSpool{objects: objects, packageID: packageID, dir: dir, host: listener.Addr().String(), path: "/" + rand.Text(), cancel: cancel, done: make(chan struct{}), media: map[string]assets.RecordingPackageObject{}, playlists: map[string][]byte{}}
	s.url = "http://" + s.host + s.path
	s.server = &http.Server{Handler: http.HandlerFunc(s.receive), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { defer close(s.done); _ = s.server.Serve(listener) }()
	go func() { <-ctx.Done(); _ = s.server.Close() }()
	return s, nil
}

func (s *OutputSpool) URL() string { return s.url }

func (s *OutputSpool) Close() error {
	s.cancel()
	err := s.server.Close()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(err, os.RemoveAll(s.dir))
}

func (s *OutputSpool) Snapshot() ([]assets.RecordingPackageObject, map[string][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, nil, s.err
	}
	objects := make([]assets.RecordingPackageObject, 0, len(s.media))
	for _, object := range s.media {
		objects = append(objects, object)
	}
	slices.SortFunc(objects, func(a, b assets.RecordingPackageObject) int { return strings.Compare(a.Path, b.Path) })
	lists := make(map[string][]byte, len(s.playlists))
	for path, data := range s.playlists {
		lists[path] = bytes.Clone(data)
	}
	return objects, lists, nil
}

func (s *OutputSpool) receive(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, s.path+"/")
	if !ok || r.Host != s.host || r.URL.RawQuery != "" || r.URL.RawPath != "" || !spoolPath.MatchString(path) {
		http.NotFound(w, r)
		return
	}
	if r.Method != "PUT" {
		w.Header().Set("Allow", "PUT")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = s.receiveObject(w, r, path)
	}
	if s.err != nil {
		http.Error(w, "recording output rejected", http.StatusUnprocessableEntity)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *OutputSpool) receiveObject(w http.ResponseWriter, r *http.Request, path string) (result error) {
	if err := r.Context().Err(); err != nil {
		return err
	}
	if strings.HasSuffix(path, ".m3u8") {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, assets.RecordingPlaylistMaxBytes))
		if err != nil || len(data) == 0 {
			return assets.ErrInvalidUpload
		}
		s.playlists[path] = data
		return nil
	}
	var disk syscall.Statfs_t
	if err := syscall.Statfs(s.dir, &disk); err != nil {
		return err
	}
	if disk.Bsize <= 0 || uint64(assets.RecordingObjectMaxBytes+(64<<20))/uint64(disk.Bsize)+1 > disk.Bavail {
		return ErrScratchCapacity
	}
	file, err := os.CreateTemp(s.dir, "object-")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, file.Close(), os.Remove(file.Name())) }()
	hash := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(file, hash), http.MaxBytesReader(w, r.Body, assets.RecordingObjectMaxBytes), make([]byte, 64<<10))
	if err != nil || n == 0 {
		return assets.ErrInvalidUpload
	}
	object := assets.RecordingPackageObject{Path: path, SizeBytes: n, SHA256: hex.EncodeToString(hash.Sum(nil))}
	if previous, exists := s.media[path]; exists {
		if previous != object {
			return assets.ErrInvalidUpload
		}
		return nil // Exact replay after a lost PUT response, never rewrite bytes.
	}
	const controlReserve = assets.RecordingInventoryMaxBytes + 4*assets.RecordingPlaylistMaxBytes
	if len(s.media) >= assets.RecordingPackageMaxObjects-4 || n > assets.RecordingPackageMaxBytes-controlReserve-s.bytes {
		return assets.ErrRecordingPackageTooLarge
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := s.objects.PutRecordingObject(r.Context(), "recordings/packages/"+s.packageID+"/staging/"+path, file, n, "video/mp4"); err != nil {
		return ErrOutputUpload
	}
	s.media[path] = object
	s.bytes += n
	return nil
}
