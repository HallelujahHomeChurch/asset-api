package recordingprocessing

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"time"

	"hhc/asset-api/internal/assets"
)

const sourceReadWindow int64 = 8 << 20

var immutableSourceKey = regexp.MustCompile(`^recording-sources/[a-f0-9]{32}/final/[a-f0-9]{32}/source$`)

type SourceObjects interface {
	Open(context.Context, string, assets.ByteRange, string) (assets.BlobDownload, error)
}

// SourceReader exposes exactly one verified immutable source on loopback. SDK
// requests retain managed-identity renewal and If-Match checks. This reader
// exposes no Blob credential and cannot proxy a caller-selected object or URL.
type SourceReader struct {
	server *http.Server
	url    string
	cancel context.CancelFunc
	done   chan struct{}
}

func NewSourceReader(ctx context.Context, objects SourceObjects, key string, size int64, etag string) (*SourceReader, error) {
	if objects == nil || !immutableSourceKey.MatchString(key) || etag == "" || assets.ValidateRecordingSourceSize(size) != nil {
		return nil, assets.ErrInvalidInput
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	path := "/" + rand.Text() + "/source.mp4"
	host := listener.Addr().String()
	slots := make(chan struct{}, 3)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host || r.URL.Path != path || r.URL.RawQuery != "" || r.URL.RawPath != "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-r.Context().Done():
			return
		}
		content := &sourceSeeker{ctx: r.Context(), objects: objects, key: key, size: size, etag: etag}
		defer content.Close()
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "no-store")
		// Stdlib handles HEAD, suffix/tail ranges, invalid ranges and lengths.
		// The seeker streams bounded SDK windows instead of buffering the source.
		http.ServeContent(w, r, "source.mp4", time.Time{}, content)
	})
	s := &SourceReader{url: "http://" + host + path, cancel: cancel, done: make(chan struct{})}
	s.server = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { defer close(s.done); _ = s.server.Serve(listener) }()
	go func() { <-ctx.Done(); _ = s.server.Close() }()
	return s, nil
}

func (s *SourceReader) URL() string  { return s.url }
func (s *SourceReader) Close() error { s.cancel(); err := s.server.Close(); <-s.done; return err }

type sourceSeeker struct {
	ctx                       context.Context
	objects                   SourceObjects
	key, etag                 string
	size, position, remaining int64
	body                      io.ReadCloser
}

func (s *sourceSeeker) Close() error {
	if s.body == nil {
		return nil
	}
	err := s.body.Close()
	s.body = nil
	s.remaining = 0
	return err
}

func (s *sourceSeeker) Seek(offset int64, whence int) (int64, error) {
	base := int64(0)
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = s.position
	case io.SeekEnd:
		base = s.size
	default:
		return 0, assets.ErrInvalidInput
	}
	// Check before addition to avoid overflow from a malicious Range header.
	if offset < -base || offset > s.size-base {
		return 0, assets.ErrInvalidInput
	}
	position := base + offset
	if position != s.position {
		if err := s.Close(); err != nil {
			return 0, assets.ErrRecordingStorageUnavailable
		}
		s.position = position
	}
	return position, nil
}

func (s *sourceSeeker) Read(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if s.position == s.size {
		return 0, io.EOF
	}
	if s.body == nil {
		count := min(sourceReadWindow, s.size-s.position)
		download, err := s.objects.Open(s.ctx, s.key, assets.ByteRange{Offset: s.position, Count: count}, s.etag)
		if err != nil {
			return 0, assets.ErrRecordingStorageUnavailable
		}
		if download.Body == nil || download.Size != count || download.TotalSize != s.size || download.ETag != s.etag {
			if download.Body != nil {
				download.Body.Close()
			}
			return 0, assets.ErrInvalidUpload
		}
		s.body, s.remaining = download.Body, count
	}
	if int64(len(p)) > s.remaining {
		p = p[:s.remaining]
	}
	n, err := s.body.Read(p)
	s.position += int64(n)
	s.remaining -= int64(n)
	if s.remaining == 0 {
		closeErr := s.Close()
		if err == nil || errors.Is(err, io.EOF) {
			err = nil
		}
		if closeErr != nil {
			err = assets.ErrRecordingStorageUnavailable
		}
	} else if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		err = assets.ErrRecordingStorageUnavailable
	}
	return n, err
}
