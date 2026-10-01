package assets

import (
	"encoding/base64"
	"errors"
	"fmt"
)

const RecordingSourceBlockBytes int64 = 16 << 20
const RecordingSourceMaxBlocks = int((RecordingSourceMaxBytes + RecordingSourceBlockBytes - 1) / RecordingSourceBlockBytes)

var ErrRecordingStorageUnavailable = errors.New("recording_source_storage_unavailable")

type RecordingSourceBlock struct {
	ID        string
	SizeBytes int64
}

type RecordingSourceBlockList struct {
	Committed   []RecordingSourceBlock
	Uncommitted []RecordingSourceBlock
	ETag        string
}

// Internal provider result, never a browser DTO or proof of a verified hash.
type RecordingSourceCopy struct {
	Key, CopyID, State, ETag string
	SizeBytes                int64
}

// Browser sources use Azure block blobs, not the legacy 597-part R2 upload.
func RecordingSourceBlockCount(size int64) (int, error) {
	if err := ValidateRecordingSourceSize(size); err != nil {
		return 0, err
	}
	return int((size + RecordingSourceBlockBytes - 1) / RecordingSourceBlockBytes), nil
}

func RecordingSourceBlockID(number int) (string, error) {
	if number < 1 || number > RecordingSourceMaxBlocks {
		return "", ErrInvalidInput
	}
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%08d", number))), nil
}

// ValidateRecordingSourceBlocks returns the complete ordered Latest block list.
// Azure may report an ID once in each set during a repeat upload. Unknown IDs,
// duplicate IDs within either set, missing blocks and incorrect tails fail closed.
// This is metadata validation, not verification of the source SHA-256/media.
func ValidateRecordingSourceBlocks(size int64, committed, uncommitted []RecordingSourceBlock) ([]string, error) {
	confirmed, err := ConfirmedRecordingSourceBlocks(size, committed, uncommitted)
	if err != nil {
		return nil, err
	}
	count, _ := RecordingSourceBlockCount(size)
	if len(confirmed) != count {
		return nil, ErrInvalidUpload
	}
	ids := make([]string, count)
	for i, number := range confirmed {
		ids[i], _ = RecordingSourceBlockID(number)
	}
	return ids, nil
}

// ConfirmedRecordingSourceBlocks validates every returned block even when a
// caller requests just one page. No untrusted or oversized block is resumable.
func ConfirmedRecordingSourceBlocks(size int64, committed, uncommitted []RecordingSourceBlock) ([]int, error) {
	count, err := RecordingSourceBlockCount(size)
	if err != nil {
		return nil, err
	}
	if len(committed) > count || len(uncommitted) > count {
		return nil, ErrInvalidUpload
	}
	ids := make([]string, count)
	expected := make(map[string]int64, count)
	for n := 1; n <= count; n++ {
		id, _ := RecordingSourceBlockID(n)
		ids[n-1] = id
		bytes := RecordingSourceBlockBytes
		if n == count {
			bytes = size - int64(count-1)*RecordingSourceBlockBytes
		}
		expected[id] = bytes
	}
	all := make(map[string]bool, count)
	for _, blocks := range [][]RecordingSourceBlock{committed, uncommitted} {
		seen := make(map[string]bool, len(blocks))
		for _, block := range blocks {
			want, exists := expected[block.ID]
			if !exists || seen[block.ID] || block.SizeBytes != want {
				return nil, ErrInvalidUpload
			}
			seen[block.ID], all[block.ID] = true, true
		}
	}
	confirmed := make([]int, 0, len(all))
	for i, id := range ids {
		if all[id] {
			confirmed = append(confirmed, i+1)
		}
	}
	return confirmed, nil
}
