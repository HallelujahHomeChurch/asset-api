package assets

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestRecordingCoverInputFormatAndResourceBounds(t *testing.T) {
	// Removing MIME, dimensions or APNG checks must admit an invalid fixture.
	encode := func(w, h int, format string) []byte {
		t.Helper()
		var b bytes.Buffer
		img := image.NewGray(image.Rect(0, 0, w, h))
		var err error
		if format == "png" {
			err = png.Encode(&b, img)
		} else {
			err = jpeg.Encode(&b, img, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	jpg := encode(1280, 720, "jpeg")
	withOrientation := func(value byte) []byte {
		// APP1 EXIF, little-endian TIFF, one SHORT orientation tag.
		tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 1, 3, 0, 1, 0, 0, 0, value, 0, 0, 0, 0, 0, 0, 0}
		payload := append([]byte("Exif\x00\x00"), tiff...)
		out := append([]byte(nil), jpg[:2]...)
		out = append(out, 0xff, 0xe1, 0, byte(len(payload)+2))
		out = append(out, payload...)
		return append(out, jpg[2:]...)
	}
	pngData := encode(320, 180, "png")
	animated := append([]byte(nil), pngData[:33]...)
	animated = append(animated, 0, 0, 0, 8, 'a', 'c', 'T', 'L', 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0)
	animated = append(animated, pngData[33:]...)
	oversizedHeader := append([]byte(nil), pngData...)
	binary.BigEndian.PutUint32(oversizedHeader[8:12], ^uint32(0))
	for _, tc := range []struct {
		name string
		data []byte
		mime string
		ok   bool
	}{
		{"jpeg", jpg, "image/jpeg", true}, {"png", pngData, "image/png", true},
		{"normal orientation", withOrientation(1), "image/jpeg", true},
		{"rotated orientation requires normalization", withOrientation(6), "image/jpeg", false},
		{"empty", nil, "image/jpeg", false}, {"spoofed", jpg, "image/png", false},
		{"unsupported", []byte("<svg/>"), "image/svg+xml", false},
		{"too many bytes", make([]byte, (5<<20)+1), "image/jpeg", false},
		{"too wide", encode(8193, 1, "png"), "image/png", false},
		{"too many pixels", encode(6000, 4001, "png"), "image/png", false},
		{"animated", animated, "image/png", false},
		{"truncated chunks", pngData[:40], "image/png", false},
		{"overflow chunk", oversizedHeader, "image/png", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ValidateRecordingCoverInput(tc.data, tc.mime)
			if (err == nil) != tc.ok {
				t.Fatalf("config=%+v err=%v want valid=%v", cfg, err, tc.ok)
			}
			if tc.ok && (cfg.Width < 1 || cfg.Height < 1) {
				t.Fatal("missing dimensions")
			}
		})
	}
}
