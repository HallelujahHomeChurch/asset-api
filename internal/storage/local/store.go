package local

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"hhc/asset-api/internal/assets"
)

type Store struct {
	root, uploadBaseURL string
	signingKey          []byte
}

func New(root, uploadBaseURL, signingKey string) (*Store, error) {
	if len(signingKey) < 16 {
		return nil, fmt.Errorf("local upload signing key must be at least 16 bytes")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	return &Store{root: root, uploadBaseURL: strings.TrimRight(uploadBaseURL, "/"), signingKey: []byte(signingKey)}, nil
}

func (s *Store) CreateUploadTarget(_ context.Context, objectKey string, maxSize int64, expiresAt time.Time) (assets.UploadTarget, error) {
	token := base64.RawURLEncoding.EncodeToString([]byte(objectKey))
	expires := strconv.FormatInt(expiresAt.Unix(), 10)
	max := strconv.FormatInt(maxSize, 10)
	signature := s.sign(token + "." + expires + "." + max)
	return assets.UploadTarget{URL: s.uploadBaseURL + "/" + token + "?expires=" + expires + "&max=" + max + "&signature=" + url.QueryEscape(signature), Method: http.MethodPut, Headers: map[string]string{"Content-Type": "application/octet-stream"}, ExpiresAt: expiresAt}, nil
}

func (s *Store) PutHandler(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	expires := r.URL.Query().Get("expires")
	maxValue := r.URL.Query().Get("max")
	provided := r.URL.Query().Get("signature")
	if !hmac.Equal([]byte(provided), []byte(s.sign(token+"."+expires+"."+maxValue))) {
		http.Error(w, "invalid upload target", http.StatusForbidden)
		return
	}
	expiresUnix, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || time.Now().After(time.Unix(expiresUnix, 0)) {
		http.Error(w, "upload target expired", http.StatusGone)
		return
	}
	maxSize, err := strconv.ParseInt(maxValue, 10, 64)
	if err != nil || maxSize <= 0 {
		http.Error(w, "invalid upload target", http.StatusForbidden)
		return
	}
	keyBytes, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		http.Error(w, "invalid upload target", http.StatusForbidden)
		return
	}
	filePath, err := s.safePath(string(keyBytes))
	if err != nil {
		http.Error(w, "invalid upload target", http.StatusForbidden)
		return
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o750); err != nil {
		http.Error(w, "storage unavailable", http.StatusInternalServerError)
		return
	}
	temporary, err := os.CreateTemp(filepath.Dir(filePath), ".upload-*")
	if err != nil {
		http.Error(w, "storage unavailable", http.StatusInternalServerError)
		return
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	written, copyErr := io.Copy(temporary, http.MaxBytesReader(w, r.Body, maxSize))
	closeErr := temporary.Close()
	if copyErr != nil || closeErr != nil || written == 0 {
		http.Error(w, "invalid upload", http.StatusBadRequest)
		return
	}
	if err := os.Rename(temporaryName, filePath); err != nil {
		http.Error(w, "storage unavailable", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Store) Inspect(_ context.Context, objectKey string) (assets.BlobProperties, error) {
	filePath, err := s.safePath(objectKey)
	if err != nil {
		return assets.BlobProperties{}, assets.ErrNotFound
	}
	file, err := os.Open(filePath)
	if os.IsNotExist(err) {
		return assets.BlobProperties{}, assets.ErrNotFound
	}
	if err != nil {
		return assets.BlobProperties{}, err
	}
	defer file.Close()
	hash := sha256.New()
	header := make([]byte, 512)
	read, readErr := io.ReadFull(file, header)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return assets.BlobProperties{}, readErr
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return assets.BlobProperties{}, err
	}
	size, err := io.Copy(hash, file)
	if err != nil {
		return assets.BlobProperties{}, err
	}
	mime := http.DetectContentType(header[:read])
	if strings.HasPrefix(string(header[:read]), "%PDF-") {
		mime = "application/pdf"
	}
	info, _ := file.Stat()
	return assets.BlobProperties{Size: size, DetectedMIMEType: mime, ChecksumSHA256: hex.EncodeToString(hash.Sum(nil)), ETag: fmt.Sprintf("%x-%x", size, info.ModTime().UnixNano())}, nil
}

func (s *Store) Open(_ context.Context, objectKey string, requested assets.ByteRange) (assets.BlobDownload, error) {
	filePath, err := s.safePath(objectKey)
	if err != nil {
		return assets.BlobDownload{}, assets.ErrNotFound
	}
	file, err := os.Open(filePath)
	if os.IsNotExist(err) {
		return assets.BlobDownload{}, assets.ErrNotFound
	}
	if err != nil {
		return assets.BlobDownload{}, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return assets.BlobDownload{}, err
	}
	var reader io.Reader = file
	size := info.Size()
	if requested.Offset > 0 || requested.Count > 0 {
		count := requested.Count
		if count <= 0 || requested.Offset+count > info.Size() {
			count = info.Size() - requested.Offset
		}
		if requested.Offset < 0 || count < 0 {
			file.Close()
			return assets.BlobDownload{}, assets.ErrInvalidInput
		}
		reader = io.NewSectionReader(file, requested.Offset, count)
		size = count
	}
	return assets.BlobDownload{Body: &readCloser{Reader: reader, Closer: file}, Size: size, TotalSize: info.Size(), ContentType: mimeFromExtension(filePath), ETag: fmt.Sprintf("%x-%x", info.Size(), info.ModTime().UnixNano()), LastModified: info.ModTime()}, nil
}

func (s *Store) Delete(_ context.Context, objectKey string) error {
	filePath, err := s.safePath(objectKey)
	if err != nil {
		return err
	}
	err = os.Remove(filePath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func (s *Store) sign(value string) string {
	mac := hmac.New(sha256.New, s.signingKey)
	_, _ = mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Store) safePath(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", assets.ErrInvalidInput
	}
	target := filepath.Join(s.root, clean)
	relative, err := filepath.Rel(s.root, target)
	if err != nil || strings.HasPrefix(relative, "..") {
		return "", assets.ErrInvalidInput
	}
	return target, nil
}
func mimeFromExtension(value string) string {
	if strings.EqualFold(filepath.Ext(value), ".pdf") {
		return "application/pdf"
	}
	return "application/octet-stream"
}

type readCloser struct {
	io.Reader
	io.Closer
}
