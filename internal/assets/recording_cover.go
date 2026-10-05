package assets

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
)

const (
	RecordingCoverInputMaxBytes  = 5 << 20
	RecordingCoverOutputMaxBytes = 1 << 20
	RecordingCoverWidth          = 1280
	RecordingCoverHeight         = 720
)

// ValidateRecordingCoverInput bounds encoded and decoded size before a worker
// decodes untrusted input. A successful header check is not a ready receipt;
// the isolated worker must fully decode and re-encode before serving any bytes.
func ValidateRecordingCoverInput(data []byte, mime string) (image.Config, error) {
	var zero image.Config
	if len(data) == 0 || len(data) > RecordingCoverInputMaxBytes || (mime != "image/jpeg" && mime != "image/png") {
		return zero, ErrInvalidUpload
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || "image/"+format != mime || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 8192 || cfg.Height > 8192 || int64(cfg.Width)*int64(cfg.Height) > 24_000_000 {
		return zero, ErrInvalidUpload
	}
	if format == "png" {
		// Go's PNG decoder accepts only the default frame of APNG. Explicitly
		// reject its animation chunks instead of silently taking that frame.
		offset := 8
		ended := false
		for offset < len(data) {
			if len(data)-offset < 12 {
				return zero, ErrInvalidUpload
			}
			size := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
			if size > uint64(len(data)-offset-12) {
				return zero, ErrInvalidUpload
			}
			kind := string(data[offset+4 : offset+8])
			if kind == "eXIf" && !coverEXIFNormal(data[offset+8:offset+8+int(size)]) {
				return zero, fmt.Errorf("%w: normalize EXIF orientation before upload", ErrInvalidUpload)
			}
			if kind == "acTL" || kind == "fcTL" || kind == "fdAT" {
				return zero, ErrInvalidUpload
			}
			offset += int(size) + 12
			if kind == "IEND" {
				ended = size == 0 && offset == len(data)
				break
			}
		}
		if !ended {
			return zero, ErrInvalidUpload
		}
	} else {
		for offset := 2; offset < len(data); {
			if data[offset] != 0xff {
				return zero, ErrInvalidUpload
			}
			for offset < len(data) && data[offset] == 0xff {
				offset++
			}
			if offset >= len(data) {
				return zero, ErrInvalidUpload
			}
			marker := data[offset]
			offset++
			if marker == 0xda {
				break
			}
			if len(data)-offset < 2 {
				return zero, ErrInvalidUpload
			}
			size := int(binary.BigEndian.Uint16(data[offset : offset+2]))
			if size < 2 || size > len(data)-offset {
				return zero, ErrInvalidUpload
			}
			payload := data[offset+2 : offset+size]
			if marker == 0xe1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")) && !coverEXIFNormal(payload[6:]) {
				return zero, fmt.Errorf("%w: normalize EXIF orientation before upload", ErrInvalidUpload)
			}
			offset += size
		}
	}
	return cfg, nil
}

// Orientation-changing EXIF must be normalized by the uploader. Malformed or
// unsupported TIFF metadata fails closed rather than stretching rotated pixels.
func coverEXIFNormal(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return false
	}
	if order.Uint16(data[2:4]) != 42 {
		return false
	}
	offset := uint64(order.Uint32(data[4:8]))
	if offset < 8 || offset+2 > uint64(len(data)) {
		return false
	}
	count := uint64(order.Uint16(data[offset : offset+2]))
	offset += 2
	if offset+count*12+4 > uint64(len(data)) {
		return false
	}
	for i := uint64(0); i < count; i++ {
		tag := data[offset+i*12 : offset+(i+1)*12]
		if order.Uint16(tag[:2]) == 0x112 && (order.Uint16(tag[2:4]) != 3 || order.Uint32(tag[4:8]) != 1 || order.Uint16(tag[8:10]) != 1) {
			return false
		}
	}
	return true
}
