package assets

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"
)

func TestValidateMediaCanonicalFormats(t *testing.T) {
	zipPayload := testZIP(t, "[Content_Types].xml", "ppt/presentation.xml")
	tests := []struct {
		name, fileName, mime string
		payload              []byte
	}{
		{"jpeg", "photo.jpeg", "image/jpeg", []byte{0xff, 0xd8, 0xff, 0xe0}},
		{"png", "image.png", "image/png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 8)...)},
		{"gif", "image.gif", "image/gif", []byte("GIF89a")},
		{"webp", "image.webp", "image/webp", []byte("RIFF\x04\x00\x00\x00WEBPVP8 ")},
		{"bmp", "image.bmp", "image/bmp", append([]byte("BM"), make([]byte, 12)...)},
		{"mp4", "movie.mp4", "video/mp4", bmff("isom")},
		{"mov", "movie.mov", "video/quicktime", bmff("qt  ")},
		{"webm", "movie.webm", "video/webm", ebml("webm")},
		{"ogv", "movie.ogv", "video/ogg", append([]byte("OggS"), append(make([]byte, 24), []byte("\x80theora")...)...)},
		{"avi", "movie.avi", "video/x-msvideo", []byte("RIFF\x04\x00\x00\x00AVI ")},
		{"mkv", "movie.mkv", "video/x-matroska", ebml("matroska")},
		{"wmv", "movie.wmv", "video/x-ms-wmv", []byte{0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11, 0xa6, 0xd9, 0x00, 0xaa, 0x00, 0x62, 0xce, 0x6c}},
		{"mp3 id3", "audio.mp3", "audio/mpeg", []byte("ID3\x04\x00\x00")},
		{"mp3 frame", "audio.mp3", "audio/mpeg", []byte{0xff, 0xfb, 0x90, 0x64}},
		{"wav", "audio.wav", "audio/wav", []byte("RIFF\x04\x00\x00\x00WAVE")},
		{"m4a", "audio.m4a", "audio/mp4", bmff("M4A ")},
		{"aac", "audio.aac", "audio/aac", []byte{0xff, 0xf1, 0x50, 0x80}},
		{"ogg vorbis", "audio.ogg", "audio/ogg", append([]byte("OggS"), append(make([]byte, 24), []byte("\x01vorbis")...)...)},
		{"pdf", "slides.pdf", "application/pdf", []byte("%PDF-1.7")},
		{"pptx", "slides.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", zipPayload},
		{"lpdeck", "slides.lpdeck", "application/vnd.hhc.presenter+json", []byte(" \n{\"version\":1}")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := test.payload
			if len(header) > 512 {
				header = header[:512]
			}
			got, err := ValidateMedia(context.Background(), test.fileName, test.mime, header, bytes.NewReader(test.payload), int64(len(test.payload)))
			if err != nil || got != test.mime {
				t.Fatalf("ValidateMedia() = %q, %v", got, err)
			}
		})
	}
}

func TestValidateMediaRejectsAliasesSpoofsAndUnsupportedFormats(t *testing.T) {
	m4aAsMP4 := bmff("M4A ")
	copy(m4aAsMP4[16:], "isom")
	tests := []struct {
		name, fileName, mime string
		payload              []byte
	}{
		{"wrong extension", "photo.png", "image/jpeg", []byte{0xff, 0xd8, 0xff}},
		{"MIME alias", "audio.mp3", "audio/mp3", []byte("ID3\x04")},
		{"retired lpdeck MIME", "slides.lpdeck", "application/vnd.librepresenter.presentation+json", []byte("{\"version\":1}")},
		{"jpeg spoof", "photo.jpg", "image/jpeg", []byte("<svg>")},
		{"AAC spoofed as MP3", "audio.mp3", "audio/mpeg", []byte{0xff, 0xf1, 0x50, 0x80}},
		{"svg", "image.svg", "image/svg+xml", []byte("<svg>")},
		{"ppt", "slides.ppt", "application/vnd.ms-powerpoint", []byte{0xd0, 0xcf, 0x11, 0xe0}},
		{"heic not mp4", "photo.mp4", "video/mp4", bmff("heic")},
		{"heif not m4a", "audio.m4a", "audio/mp4", bmff("heif")},
		{"M4A not MP4 video", "audio.mp4", "video/mp4", m4aAsMP4},
		{"EBML text is not DocType", "movie.webm", "video/webm", []byte("\x1a\x45\xdf\xa3xxxxwebm")},
		{"pptx missing content types", "slides.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", testZIP(t, "ppt/presentation.xml")},
		{"lpdeck zip", "slides.lpdeck", "application/vnd.hhc.presenter+json", testZIP(t, "deck.json")},
		{"lpdeck malformed", "slides.lpdeck", "application/vnd.hhc.presenter+json", []byte("{not-json")},
		{"lpdeck trailing value", "slides.lpdeck", "application/vnd.hhc.presenter+json", []byte("{\"slides\":[]} {\"second\":true}")},
		{"lpdeck trailing garbage", "slides.lpdeck", "application/vnd.hhc.presenter+json", []byte("{\"slides\":[]} garbage")},
		{"lpdeck array", "slides.lpdeck", "application/vnd.hhc.presenter+json", []byte("[{\"slides\":[]}]")},
		{"lpdeck scalar", "slides.lpdeck", "application/vnd.hhc.presenter+json", []byte("\"slides\"")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ValidateMedia(context.Background(), test.fileName, test.mime, test.payload, bytes.NewReader(test.payload), int64(len(test.payload))); err == nil {
				t.Fatal("ValidateMedia accepted invalid media")
			}
		})
	}
}

func TestValidateMediaPPTXIsBoundedCancelableAndDoesNotOwnCleanup(t *testing.T) {
	payload := testZIP(t, "[Content_Types].xml", "ppt/presentation.xml")
	limited := &boundedReaderAt{value: payload, maxEnd: int64(len(payload))}
	if _, err := ValidateMedia(context.Background(), "slides.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", payload[:min(len(payload), 512)], limited, int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if limited.exceeded {
		t.Fatal("validator read past declared size")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ValidateMedia(canceled, "slides.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", payload, bytes.NewReader(payload), int64(len(payload))); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
	file, err := os.CreateTemp("", "media-validation-*")
	if err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateMedia(context.Background(), "slides.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", payload, file, int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); err != nil {
		t.Fatalf("validator removed caller-owned file: %v", err)
	}
}

func TestValidateMediaLPDeckIsBoundedAndCancelable(t *testing.T) {
	payload := []byte("{\"slides\":[{\"name\":\"one\"}]}")
	limited := &boundedReaderAt{value: append(payload, []byte(" {\"second\":true}")...), maxEnd: int64(len(payload))}
	got, err := ValidateMedia(context.Background(), "slides.lpdeck", "application/vnd.hhc.presenter+json", payload, limited, int64(len(payload)))
	if err != nil || got != "application/vnd.hhc.presenter+json" {
		t.Fatalf("ValidateMedia() = %q, %v", got, err)
	}
	if limited.exceeded {
		t.Fatal("LPDeck validation read past declared size")
	}
	ctx, cancel := context.WithCancel(context.Background())
	canceling := &cancelingReaderAt{value: payload, cancel: cancel}
	if _, err := ValidateMedia(ctx, "slides.lpdeck", "application/vnd.hhc.presenter+json", payload, canceling, int64(len(payload))); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
}

func testZIP(t *testing.T, names ...string) []byte {
	t.Helper()
	var value bytes.Buffer
	w := zip.NewWriter(&value)
	for _, name := range names {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(f, "value"); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return value.Bytes()
}

func bmff(brand string) []byte {
	return append([]byte{0, 0, 0, 20}, append([]byte("ftyp"+brand), make([]byte, 8)...)...)
}
func ebml(docType string) []byte {
	return append([]byte{0x1a, 0x45, 0xdf, 0xa3, 0x42, 0x82, 0x80 | byte(len(docType))}, []byte(docType)...)
}

type boundedReaderAt struct {
	value    []byte
	maxEnd   int64
	exceeded bool
}

type cancelingReaderAt struct {
	value  []byte
	cancel context.CancelFunc
}

func (r *cancelingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.value)) {
		return 0, io.EOF
	}
	n := copy(p, r.value[off:])
	r.cancel()
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *boundedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > r.maxEnd {
		r.exceeded = true
	}
	if off >= int64(len(r.value)) {
		return 0, io.EOF
	}
	n := copy(p, r.value[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// LINE returns audio/mp4 content with isom/iso2/mp41 brands, rather than M4A.
func TestValidateMediaGenericMP4Audio(t *testing.T) {
	box := func(kind string, body []byte) []byte {
		value := make([]byte, 8+len(body))
		binary.BigEndian.PutUint32(value, uint32(len(value)))
		copy(value[4:8], kind)
		copy(value[8:], body)
		return value
	}
	track := func(handler string) []byte {
		return box("trak", box("mdia", box("hdlr", append(make([]byte, 8), []byte(handler)...))))
	}
	ftyp := box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2mp41"))
	audio := append(append([]byte{}, ftyp...), box("moov", track("soun"))...)
	extended := make([]byte, 16)
	binary.BigEndian.PutUint32(extended, 1)
	copy(extended[4:8], "moov")
	binary.BigEndian.PutUint64(extended[8:], uint64(16+len(track("soun"))))
	extended = append(extended, track("soun")...)
	toEnd := box("moov", track("soun"))
	clear(toEnd[:4])
	oversized := append([]byte{}, extended...)
	binary.BigEndian.PutUint64(oversized[8:], ^uint64(0))

	cases := []struct {
		name  string
		body  []byte
		valid bool
	}{
		{"LINE audio", audio, true},
		{"extended box", append(append([]byte{}, ftyp...), extended...), true},
		{"box to end", append(append([]byte{}, ftyp...), toEnd...), true},
		{"overflowing box", append(append([]byte{}, ftyp...), oversized...), false},
		{"box work limit", append(bytes.Repeat(box("free", nil), 10001), audio...), false},
		{"video", append(append([]byte{}, ftyp...), box("moov", track("vide"))...), false},
		{"mixed tracks", append(append([]byte{}, ftyp...), box("moov", append(track("soun"), track("vide")...))...), false},
		{"missing tracks", ftyp, false},
		{"empty extra track", append(append([]byte{}, ftyp...), box("moov", append(track("soun"), box("trak", nil)...))...), false},
		{"handler text in media data", append(append([]byte{}, ftyp...), box("mdat", track("soun"))...), false},
		{"truncated box", audio[:len(audio)-1], false},
		{"brand text outside ftyp", append(box("ftyp", []byte("xxxx\x00\x00\x00\x00")), box("mdat", []byte("M4A "))...), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &boundedReaderAt{value: tc.body, maxEnd: int64(len(tc.body))}
			_, err := ValidateMedia(context.Background(), "audio.m4a", "audio/mp4", tc.body[:min(len(tc.body), 512)], reader, int64(len(tc.body)))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
			if reader.exceeded {
				t.Fatal("read beyond content")
			}
		})
	}
}
