package recordingvalidation

import (
	"encoding/binary"
	"io"
	"os"

	"hhc/asset-api/internal/assets"
)

type fragmentBox struct {
	kind   string
	body   *io.SectionReader
	budget *int
}

// Read box headers only: mdat payloads stay on disk and metadata stays bounded.
func fragmentBoxes(r io.ReaderAt, size int64, budget *int) ([]fragmentBox, error) {
	var boxes []fragmentBox
	for offset := int64(0); offset < size; {
		*budget--
		var header [16]byte
		if *budget <= 0 || size-offset < 8 {
			return nil, assets.ErrInvalidUpload
		}
		if _, err := r.ReadAt(header[:8], offset); err != nil {
			return nil, err
		}
		length, head := uint64(binary.BigEndian.Uint32(header[:4])), int64(8)
		if length == 1 {
			if size-offset < 16 {
				return nil, assets.ErrInvalidUpload
			}
			if _, err := r.ReadAt(header[8:], offset+8); err != nil {
				return nil, err
			}
			length, head = binary.BigEndian.Uint64(header[8:]), 16
		} else if length == 0 {
			length = uint64(size - offset)
		}
		if length < uint64(head) || length > uint64(size-offset) {
			return nil, assets.ErrInvalidUpload
		}
		boxes = append(boxes, fragmentBox{string(header[4:8]), io.NewSectionReader(r, offset+head, int64(length)-head), budget})
		offset += int64(length)
	}
	return boxes, nil
}

func (b fragmentBox) children() ([]fragmentBox, error) {
	return fragmentBoxes(b.body, b.body.Size(), b.budget)
}

func (b fragmentBox) metadata() ([]byte, error) {
	if b.body.Size() > 64<<10 {
		return nil, assets.ErrInvalidUpload
	}
	return io.ReadAll(b.body)
}

// Older ffprobe replaces AAC sample durations with the codec's nominal duration.
func fragmentSampleDurations(path string) (map[uint32][]uint32, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	budget := 4096
	boxes, err := fragmentBoxes(file, info.Size(), &budget)
	if err != nil {
		return nil, err
	}
	defaults := map[uint32]uint32{}
	for _, box := range boxes {
		if box.kind != "moov" {
			continue
		}
		children, err := box.children()
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if child.kind != "mvex" {
				continue
			}
			entries, err := child.children()
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if entry.kind == "trex" {
					data, err := entry.metadata()
					if err != nil || len(data) != 24 || binary.BigEndian.Uint32(data[:4]) != 0 {
						return nil, assets.ErrInvalidUpload
					}
					track := binary.BigEndian.Uint32(data[4:8])
					if _, exists := defaults[track]; track == 0 || exists {
						return nil, assets.ErrInvalidUpload
					}
					defaults[track] = binary.BigEndian.Uint32(data[12:16])
				}
			}
		}
	}
	result := map[uint32][]uint32{}
	total := 0
	for _, box := range boxes {
		if box.kind != "moof" {
			continue
		}
		children, err := box.children()
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if child.kind != "traf" {
				continue
			}
			entries, err := child.children()
			if err != nil {
				return nil, err
			}
			var track, duration uint32
			for _, entry := range entries {
				if entry.kind != "tfhd" {
					continue
				}
				data, err := entry.metadata()
				if err != nil || len(data) < 8 || data[0] != 0 || track != 0 {
					return nil, assets.ErrInvalidUpload
				}
				flags := binary.BigEndian.Uint32(data[:4])
				if flags & ^uint32(0x02003b) != 0 {
					return nil, assets.ErrInvalidUpload
				}
				track = binary.BigEndian.Uint32(data[4:8])
				duration = defaults[track]
				offset := 8
				for _, field := range []struct {
					flag uint32
					size int
				}{{1, 8}, {2, 4}, {8, 4}, {16, 4}, {32, 4}} {
					if flags&field.flag != 0 {
						if len(data)-offset < field.size {
							return nil, assets.ErrInvalidUpload
						}
						if field.flag == 8 {
							duration = binary.BigEndian.Uint32(data[offset:])
						}
						offset += field.size
					}
				}
				if offset != len(data) {
					return nil, assets.ErrInvalidUpload
				}
			}
			if track == 0 {
				return nil, assets.ErrInvalidUpload
			}
			for _, entry := range entries {
				if entry.kind != "trun" {
					continue
				}
				data, err := entry.metadata()
				if err != nil || len(data) < 8 || data[0] > 1 {
					return nil, assets.ErrInvalidUpload
				}
				flags, count := binary.BigEndian.Uint32(data[:4])&0xffffff, binary.BigEndian.Uint32(data[4:8])
				if flags & ^uint32(0xf05) != 0 || count == 0 || count > 4096 || total+int(count) > 4096 {
					return nil, assets.ErrInvalidUpload
				}
				offset := 8
				if flags&1 != 0 {
					offset += 4
				}
				if flags&4 != 0 {
					offset += 4
				}
				for range count {
					value := duration
					for _, flag := range []uint32{0x100, 0x200, 0x400, 0x800} {
						if flags&flag != 0 {
							if len(data)-offset < 4 {
								return nil, assets.ErrInvalidUpload
							}
							if flag == 0x100 {
								value = binary.BigEndian.Uint32(data[offset:])
							}
							offset += 4
						}
					}
					if value == 0 {
						return nil, assets.ErrInvalidUpload
					}
					result[track] = append(result[track], value)
				}
				if offset != len(data) {
					return nil, assets.ErrInvalidUpload
				}
				total += int(count)
			}
		}
	}
	return result, nil
}
