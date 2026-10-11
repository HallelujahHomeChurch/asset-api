package assets

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func personalSourceCorpus(t *testing.T) []struct {
	Name     string          `json:"name"`
	Valid    bool            `json:"valid"`
	Document json.RawMessage `json:"document"`
} {
	t.Helper()
	raw, err := os.ReadFile("testdata/pptx-source/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Name     string          `json:"name"`
		Valid    bool            `json:"valid"`
		Document json.RawMessage `json:"document"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestPersonalDeckSourceCorpus(t *testing.T) {
	for _, entry := range personalSourceCorpus(t) {
		t.Run(entry.Name, func(t *testing.T) {
			err := ValidatePersonalDeck(context.Background(), bytes.NewReader(entry.Document))
			if (err == nil) != entry.Valid {
				t.Fatalf("valid=%v err=%v", entry.Valid, err)
			}
		})
	}
}

func TestPersonalDeckSourceNativeV2AndCancellation(t *testing.T) {
	native := []byte(`{"schemaVersion":2,"id":"d","name":"Native","width":1920,"height":1080,"slideOrder":[],"slides":{},"assets":{}}`)
	if err := ValidatePersonalDeck(context.Background(), bytes.NewReader(native)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidatePersonalDeck(ctx, bytes.NewReader(personalSourceCorpus(t)[0].Document)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}

func TestPersonalDeckSourceNullFieldsAreNotLegacyOmission(t *testing.T) {
	for _, field := range []string{`"pptxSource":null,`, `"pptxSource":{"version":1,"base64":"","sha256":""},`} {
		payload := []byte(`{` + field + `"schemaVersion":1,"id":"d","name":"Legacy","width":1920,"height":1080,"slideOrder":[],"slides":{},"assets":{}}`)
		if err := ValidatePersonalDeck(context.Background(), bytes.NewReader(payload)); err == nil {
			t.Fatal("source record ignored on v1")
		}
	}
}

func TestPersonalDeckSourceCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidatePersonalDeck(ctx, bytes.NewReader(personalSourceCorpus(t)[0].Document)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled=%v", err)
	}
}

func TestPersonalDeckSourceUnsafePackages(t *testing.T) {
	var original map[string]json.RawMessage
	if err := json.Unmarshal(personalSourceCorpus(t)[0].Document, &original); err != nil {
		t.Fatal(err)
	}
	var retained struct {
		Version int    `json:"version"`
		Base64  string `json:"base64"`
		SHA256  string `json:"sha256"`
	}
	if err := json.Unmarshal(original["pptxSource"], &retained); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(retained.Base64)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	parts := map[string][]byte{}
	for _, file := range reader.File {
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		parts[file.Name] = data
	}
	for _, mutation := range []string{"depth", "DTD", "macro", "encrypted", "CRC", "truncated", "unsafe-path", "duplicate-name", "missing-root", "size", "count", "zip64-extra", "base64", "digest"} {
		t.Run(mutation, func(t *testing.T) {
			var output bytes.Buffer
			writer := zip.NewWriter(&output)
			for name, data := range parts {
				if mutation == "depth" && name == "ppt/slides/slide1.xml" {
					data = bytes.Replace(data, []byte("<p:spTree>"), []byte("<p:spTree>"+strings.Repeat("<x>", 128)+strings.Repeat("</x>", 128)), 1)
				}
				if mutation == "DTD" && name == "ppt/slides/slide1.xml" {
					data = append([]byte(`<!DOCTYPE p:sld [<!ENTITY x "expanded">]>`), data...)
				}
				if mutation == "missing-root" && name == "ppt/presentation.xml" {
					continue
				}
				w, err := writer.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write(data); err != nil {
					t.Fatal(err)
				}
			}
			if mutation == "macro" || mutation == "unsafe-path" || mutation == "duplicate-name" {
				name := "ppt/vbaProject.bin"
				if mutation == "unsafe-path" {
					name = "../evil.xml"
				}
				if mutation == "duplicate-name" {
					name = "ppt/slides/slide1.xml"
				}
				w, err := writer.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				w.Write([]byte("payload"))
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			modified := append([]byte(nil), output.Bytes()...)
			central := bytes.Index(modified, []byte{0x50, 0x4b, 0x01, 0x02})
			if mutation == "encrypted" {
				binary.LittleEndian.PutUint16(modified[central+8:], 9)
				binary.LittleEndian.PutUint16(modified[int(binary.LittleEndian.Uint32(modified[central+42:]))+6:], 9)
			}
			if mutation == "CRC" {
				binary.LittleEndian.PutUint32(modified[central+16:], 0)
			}
			if mutation == "size" {
				binary.LittleEndian.PutUint32(modified[central+24:], 33554433)
			}
			if mutation == "count" {
				binary.LittleEndian.PutUint16(modified[len(modified)-22+8:], 4001)
				binary.LittleEndian.PutUint16(modified[len(modified)-22+10:], 4001)
			}
			if mutation == "zip64-extra" {
				nameSize := int(binary.LittleEndian.Uint16(modified[central+28:]))
				extraSize := binary.LittleEndian.Uint16(modified[central+30:])
				insertion := central + 46 + nameSize
				directorySize := binary.LittleEndian.Uint32(modified[len(modified)-22+12:])
				expanded := make([]byte, len(modified)+4)
				copy(expanded, modified[:insertion])
				copy(expanded[insertion:], []byte{1, 0, 0, 0})
				copy(expanded[insertion+4:], modified[insertion:])
				binary.LittleEndian.PutUint16(expanded[central+30:], extraSize+4)
				binary.LittleEndian.PutUint32(expanded[len(expanded)-22+12:], directorySize+4)
				modified = expanded
			}
			if mutation == "truncated" {
				modified = modified[:len(modified)-10]
			}
			hash := sha256.Sum256(modified)
			retained.Base64 = base64.StdEncoding.EncodeToString(modified)
			retained.SHA256 = hex.EncodeToString(hash[:])
			if mutation == "base64" {
				retained.Base64 += "\n"
			}
			if mutation == "digest" {
				retained.SHA256 = strings.Repeat("0", 64)
			}
			original["pptxSource"], err = json.Marshal(retained)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidatePersonalDeck(context.Background(), bytes.NewReader(body)); err == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}
}

type personalSourceCancellingReader struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r personalSourceCancellingReader) Read(data []byte) (int, error) {
	r.cancel()
	return r.reader.Read(data)
}
func TestPersonalDeckSourceCancellationDuringRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := personalSourceCancellingReader{bytes.NewReader(personalSourceCorpus(t)[0].Document), cancel}
	if err := ValidatePersonalDeck(ctx, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-read cancellation=%v", err)
	}
}

func TestPersonalPptxXMLWithUTF8BOM(t *testing.T) {
	xml := append([]byte{0xef, 0xbb, 0xbf}, []byte(`<p:presentation xmlns:p="`+personalPptxPresentationNS+`"/>`)...)
	if _, err := personalPptxXMLRead(context.Background(), xml); err != nil {
		t.Fatalf("valid BOM rejected: %v", err)
	}
}

func TestPersonalPptxDirectoryExpandedLimits(t *testing.T) {
	for _, test := range []struct {
		name  string
		count int
		size  uint32
		media bool
		valid bool
	}{
		{"entry-at-limit", 1, 33554432, false, true},
		{"entry-over-limit", 1, 33554433, false, false},
		{"total-at-limit", 8, 33554432, false, true},
		{"total-over-limit", 9, 33554432, false, false},
		{"media-at-limit", 6, 33554432, true, true},
		{"media-over-limit", 7, 33554432, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			writer := zip.NewWriter(&output)
			for index := range test.count {
				name := string(rune('a'+index)) + ".bin"
				if test.media {
					name = "ppt/media/" + name
				}
				entry, err := writer.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				entry.Write([]byte("x"))
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			data := output.Bytes()
			for offset := 0; offset+46 <= len(data); offset++ {
				if binary.LittleEndian.Uint32(data[offset:]) == 0x02014b50 {
					binary.LittleEndian.PutUint32(data[offset+24:], test.size)
				}
			}
			_, err := personalPptxDirectory(data)
			if (err == nil) != test.valid {
				t.Fatalf("declared ZIP limit result=%v want valid=%v", err, test.valid)
			}
		})
	}
}
