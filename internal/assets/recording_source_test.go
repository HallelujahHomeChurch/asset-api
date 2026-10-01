package assets

import (
	"math"
	"testing"
)

func TestSourceBlocksSupportFiftyGBBeyondLegacyMultipartPage(t *testing.T) {
	for number, want := range map[int]string{1: "MDAwMDAwMDE=", 2981: "MDAwMDI5ODE="} {
		if got, err := RecordingSourceBlockID(number); err != nil || got != want {
			t.Fatalf("block ID golden: %d %s %v", number, got, err)
		}
	}
	for _, size := range []int64{20_000_000_000, 30_000_000_000, RecordingSourceMaxBytes} {
		count, err := RecordingSourceBlockCount(size)
		if err != nil || count <= 1000 {
			t.Fatalf("size=%d count=%d err=%v", size, count, err)
		}
		blocks := make([]RecordingSourceBlock, count)
		for n := 1; n <= count; n++ {
			id, err := RecordingSourceBlockID(n)
			if err != nil {
				t.Fatal(err)
			}
			bytes := RecordingSourceBlockBytes
			if n == count {
				bytes = size - int64(count-1)*RecordingSourceBlockBytes
			}
			blocks[n-1] = RecordingSourceBlock{ID: id, SizeBytes: bytes}
		}
		ids, err := ValidateRecordingSourceBlocks(size, blocks, nil)
		if err != nil || len(ids) != count {
			t.Fatalf("full source closure %d: %v", size, err)
		}
		ids, err = ValidateRecordingSourceBlocks(size, blocks[:count/2], blocks[count/2:])
		if err != nil || len(ids) != count {
			t.Fatalf("resumed block closure: %v", err)
		}
	}
	for _, size := range []int64{0, -1, RecordingSourceMaxBytes + 1, math.MaxInt64} {
		if _, err := RecordingSourceBlockCount(size); err == nil {
			t.Fatalf("accepted size %d", size)
		}
	}
	if count, err := RecordingSourceBlockCount(RecordingSourceMaxBytes); err != nil || count != 2981 {
		t.Fatalf("50GB limit: %d %v", count, err)
	}
}

func TestSourceBlockClosureRejectsMissingExtraDuplicateAndWrongSizes(t *testing.T) {
	id, _ := RecordingSourceBlockID(1)
	second, _ := RecordingSourceBlockID(2)
	for _, blocks := range [][]RecordingSourceBlock{nil, {{ID: id, SizeBytes: 4}}, {{ID: id, SizeBytes: 6}}, {{ID: second, SizeBytes: 5}}, {{ID: "invalid", SizeBytes: 5}}, {{ID: id, SizeBytes: 5}, {ID: id, SizeBytes: 5}}} {
		if _, err := ValidateRecordingSourceBlocks(5, nil, blocks); err == nil {
			t.Fatalf("accepted bad closure %+v", blocks)
		}
	}
	if _, err := ValidateRecordingSourceBlocks(5, []RecordingSourceBlock{{ID: id, SizeBytes: 5}}, []RecordingSourceBlock{{ID: id, SizeBytes: 5}}); err != nil {
		t.Fatal("valid latest block replay rejected")
	}
	for _, number := range []int{0, -1, 2982, math.MaxInt32} {
		if _, err := RecordingSourceBlockID(number); err == nil {
			t.Fatalf("accepted block number %d", number)
		}
	}
}
