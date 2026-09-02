package assets

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

var mediaExtensions = map[string][]string{
	"image/jpeg": {".jpg", ".jpeg"}, "image/png": {".png"}, "image/gif": {".gif"},
	"image/webp": {".webp"}, "image/bmp": {".bmp"}, "video/mp4": {".mp4"},
	"video/quicktime": {".mov"}, "video/webm": {".webm"}, "video/ogg": {".ogv"},
	"video/x-msvideo": {".avi"}, "video/x-matroska": {".mkv"}, "video/x-ms-wmv": {".wmv"},
	"audio/mpeg": {".mp3"}, "audio/wav": {".wav"}, "audio/mp4": {".m4a"},
	"audio/aac": {".aac"}, "audio/ogg": {".ogg"}, "application/pdf": {".pdf"},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {".pptx"},
	"application/vnd.hhc.presenter+json":                                        {".lpdeck"},
	"application/vnd.ms-powerpoint":                                             {".ppt"},
	"application/vnd.apple.keynote":                                             {".key"},
	"application/vnd.oasis.opendocument.presentation":                           {".odp"},
	"application/msword":                                                        {".doc"},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   {".docx"},
	"application/vnd.ms-excel":                                                  {".xls"},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         {".xlsx"},
	"application/zip":                                                           {".zip"}, "text/plain": {".txt"}, "text/markdown": {".md", ".markdown"},
}

func ValidateMedia(ctx context.Context, fileName, expectedMIME string, header []byte, content io.ReaderAt, size int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if size <= 0 || !extensionAllowed(fileName, expectedMIME) {
		return "", ErrInvalidUpload
	}
	valid := false
	switch expectedMIME {
	case "image/jpeg":
		valid = len(header) >= 3 && bytes.Equal(header[:3], []byte{0xff, 0xd8, 0xff})
	case "image/png":
		valid = bytes.HasPrefix(header, []byte("\x89PNG\r\n\x1a\n"))
	case "image/gif":
		valid = bytes.HasPrefix(header, []byte("GIF87a")) || bytes.HasPrefix(header, []byte("GIF89a"))
	case "image/webp":
		valid = len(header) >= 16 && bytes.Equal(header[:4], []byte("RIFF")) && bytes.Equal(header[8:12], []byte("WEBP")) && bytes.HasPrefix(header[12:], []byte("VP8"))
	case "image/bmp":
		valid = bytes.HasPrefix(header, []byte("BM"))
	case "video/mp4":
		valid = bmffHasBrand(header, "isom", "iso2", "mp41", "mp42", "avc1", "M4V ") && !bmffHasBrand(header, "M4A ", "M4B ") && !bmffHasHEIFBrand(header)
	case "video/quicktime":
		valid = bmffHasBrand(header, "qt  ")
	case "video/webm":
		valid = validEBML(header, "webm")
	case "video/ogg":
		valid = bytes.HasPrefix(header, []byte("OggS")) && bytes.Contains(header, []byte("theora"))
	case "video/x-msvideo":
		valid = len(header) >= 12 && bytes.Equal(header[:4], []byte("RIFF")) && bytes.Equal(header[8:12], []byte("AVI "))
	case "video/x-matroska":
		valid = validEBML(header, "matroska")
	case "video/x-ms-wmv":
		valid = bytes.HasPrefix(header, []byte{0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11, 0xa6, 0xd9, 0x00, 0xaa, 0x00, 0x62, 0xce, 0x6c})
	case "audio/mpeg":
		valid = bytes.HasPrefix(header, []byte("ID3")) || validMP3Frame(header)
	case "audio/wav":
		valid = len(header) >= 12 && bytes.Equal(header[:4], []byte("RIFF")) && bytes.Equal(header[8:12], []byte("WAVE"))
	case "audio/mp4":
		valid = bmffHasBrand(header, "M4A ", "M4B ") && !bmffHasHEIFBrand(header)
	case "audio/aac":
		valid = bytes.HasPrefix(header, []byte("ADIF")) || len(header) >= 2 && header[0] == 0xff && header[1]&0xf6 == 0xf0
	case "audio/ogg":
		valid = bytes.HasPrefix(header, []byte("OggS")) && (bytes.Contains(header, []byte("vorbis")) || bytes.Contains(header, []byte("OpusHead")) || bytes.Contains(header, []byte("Speex")) || bytes.Contains(header, []byte("fLaC")))
	case "application/pdf":
		valid = bytes.HasPrefix(header, []byte("%PDF-"))
	case "application/vnd.hhc.presenter+json":
		valid = validJSONObject(ctx, content, size)
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		valid = validZIP(ctx, content, size, "[Content_Types].xml", "ppt/presentation.xml")
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		valid = validZIP(ctx, content, size, "[Content_Types].xml", "word/document.xml")
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		valid = validZIP(ctx, content, size, "[Content_Types].xml", "xl/workbook.xml")
	case "application/zip":
		valid = validZIP(ctx, content, size)
	case "application/vnd.apple.keynote":
		valid = validZIPAny(ctx, content, size, "Index/Document.iwa", "index.apxl")
	case "application/vnd.oasis.opendocument.presentation":
		valid = validZIP(ctx, content, size, "mimetype", "content.xml")
	case "application/vnd.ms-powerpoint", "application/msword", "application/vnd.ms-excel":
		valid = bytes.HasPrefix(header, []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1})
	case "text/plain", "text/markdown":
		valid = utf8.Valid(header) && !bytes.ContainsRune(header, 0)
	}
	if !valid {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", ErrInvalidUpload
	}
	return expectedMIME, nil
}

func requiresContentReader(mime string) bool {
	switch mime {
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.apple.keynote",
		"application/vnd.oasis.opendocument.presentation",
		"application/zip",
		"application/vnd.hhc.presenter+json":
		return true
	default:
		return false
	}
}

func validJSONObject(ctx context.Context, content io.ReaderAt, size int64) bool {
	if content == nil || size <= 0 {
		return false
	}
	decoder := json.NewDecoder(&contextReader{ctx: ctx, reader: io.NewSectionReader(content, 0, size)})
	token, err := decoder.Token()
	delim, ok := token.(json.Delim)
	if err != nil || !ok || delim != '{' {
		return false
	}
	depth := 1
	for depth > 0 {
		if ctx.Err() != nil {
			return false
		}
		token, err = decoder.Token()
		if err != nil {
			return false
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	if ctx.Err() != nil {
		return false
	}
	_, err = decoder.Token()
	return err == io.EOF
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(value []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(value)
	}
}

func extensionAllowed(fileName, mime string) bool {
	ext := strings.ToLower(path.Ext(strings.TrimSpace(fileName)))
	for _, allowed := range mediaExtensions[mime] {
		if ext == allowed {
			return true
		}
	}
	return false
}

func bmffHasBrand(header []byte, allowed ...string) bool {
	if len(header) < 12 || !bytes.Equal(header[4:8], []byte("ftyp")) {
		return false
	}
	for offset := 8; offset+4 <= len(header); offset += 4 {
		brand := string(header[offset : offset+4])
		for _, candidate := range allowed {
			if brand == candidate {
				return true
			}
		}
	}
	return false
}

func bmffHasHEIFBrand(header []byte) bool {
	return bmffHasBrand(header, "heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1", "avif", "avis")
}

func validEBML(header []byte, docType string) bool {
	element := append([]byte{0x42, 0x82, 0x80 | byte(len(docType))}, []byte(docType)...)
	return bytes.HasPrefix(header, []byte{0x1a, 0x45, 0xdf, 0xa3}) && bytes.Contains(header, element)
}

func validMP3Frame(header []byte) bool {
	if len(header) < 4 || header[0] != 0xff || header[1]&0xe0 != 0xe0 {
		return false
	}
	version, layer := (header[1]>>3)&3, (header[1]>>1)&3
	bitrate, sampleRate := (header[2]>>4)&15, (header[2]>>2)&3
	return version != 1 && layer != 0 && bitrate != 0 && bitrate != 15 && sampleRate != 3
}

func validZIP(ctx context.Context, content io.ReaderAt, size int64, required ...string) bool {
	if content == nil || size <= 0 {
		return false
	}
	reader, err := zip.NewReader(content, size)
	if err != nil {
		return false
	}
	found := make(map[string]bool, len(required))
	for _, file := range reader.File {
		if ctx.Err() != nil {
			return false
		}
		name := strings.TrimPrefix(file.Name, "/")
		for _, requiredName := range required {
			if name == requiredName {
				found[requiredName] = true
			}
		}
	}
	return len(found) == len(required)
}

func validZIPAny(ctx context.Context, content io.ReaderAt, size int64, names ...string) bool {
	if content == nil || size <= 0 {
		return false
	}
	reader, err := zip.NewReader(content, size)
	if err != nil {
		return false
	}
	for _, file := range reader.File {
		if ctx.Err() != nil {
			return false
		}
		name := strings.TrimPrefix(file.Name, "/")
		for _, candidate := range names {
			if name == candidate || strings.HasPrefix(name, candidate) {
				return true
			}
		}
	}
	return false
}
