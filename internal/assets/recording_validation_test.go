package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"
)

type interruptedRecordingReader struct{ err error }

func (r interruptedRecordingReader) Read([]byte) (int, error) { return 0, r.err }

func TestRecordingValidationPreservesTransportFailureForRetry(t *testing.T) {
	transportError := errors.New("R2 stream interrupted")
	probe := []byte(`{"streams":[{"codec_type":"video","codec_name":"h264"},{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"9000"}}`)
	_, err := ValidateRecordingStream(interruptedRecordingReader{transportError}, 100, hex.EncodeToString(make([]byte, 32)), probe)
	if !errors.Is(err, transportError) {
		t.Fatalf("transport error lost: %v", err)
	}
}

func mp4Box(kind string, payload []byte) []byte {
	box := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(box, uint32(len(box)))
	copy(box[4:8], kind)
	copy(box[8:], payload)
	return box
}

func TestRecordingValidationStreamsIntegrityAndRequiresFastStart(t *testing.T) {
	valid := bytes.Join([][]byte{
		mp4Box("ftyp", []byte("isom0000")),
		mp4Box("moov", []byte("metadata")),
		mp4Box("mdat", []byte("video bytes")),
	}, nil)
	sum := sha256.Sum256(valid)
	checksum := hex.EncodeToString(sum[:])
	probe := []byte(`{"streams":[{"codec_type":"video","codec_name":"h264"},{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"9000.25"}}`)
	duration, err := ValidateRecordingStream(bytes.NewReader(valid), int64(len(valid)), checksum, probe)
	if err != nil || duration != 9000.25 {
		t.Fatalf("valid MP4: duration=%v err=%v", duration, err)
	}
	longProbe := []byte(`{"streams":[{"codec_type":"video","codec_name":"h264"},{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"90000"}}`)
	if _, err := ValidateRecordingStream(bytes.NewReader(valid), int64(len(valid)), checksum, longProbe); err != nil {
		t.Fatalf("size, not duration, is the product limit: %v", err)
	}
	for _, tt := range []struct {
		name  string
		data  []byte
		size  int64
		hash  string
		probe []byte
	}{
		{"truncated", valid[:len(valid)-1], int64(len(valid)), checksum, probe},
		{"wrong checksum", valid, int64(len(valid)), hex.EncodeToString(make([]byte, 32)), probe},
		{"extra bytes", append(bytes.Clone(valid), 1), int64(len(valid)), checksum, probe},
		{"unsupported codec", valid, int64(len(valid)), checksum, []byte(`{"streams":[{"codec_type":"video","codec_name":"hevc"},{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"9000.25"}}`)},
		{"invalid duration", valid, int64(len(valid)), checksum, []byte(`{"streams":[{"codec_type":"video","codec_name":"h264"},{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"NaN"}}`)},
		{"no fast start", bytes.Join([][]byte{mp4Box("ftyp", []byte("isom0000")), mp4Box("mdat", []byte("bytes")), mp4Box("moov", []byte("metadata"))}, nil), 0, "", probe},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.size == 0 {
				tt.size = int64(len(tt.data))
				sum := sha256.Sum256(tt.data)
				tt.hash = hex.EncodeToString(sum[:])
			}
			if _, err := ValidateRecordingStream(bytes.NewReader(tt.data), tt.size, tt.hash, tt.probe); err == nil {
				t.Fatal("invalid recording must not be ready")
			}
		})
	}
}
