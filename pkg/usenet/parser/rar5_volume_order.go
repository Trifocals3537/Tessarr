package parser

import (
	"bytes"
	"fmt"
	"math"

	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/Trifocals3537/tessarr/pkg/usenet/types"
)

type rar5VolumeInfo struct {
	number uint64
	multi  bool
	next   bool
}

// Header CRC validation (and decryption, if needed) precedes this call. Keep
// optional extra records outside the required main/end fields: an extra record
// must never stand in for a truncated volume number.
func (r *parseRAR5StreamResult) recordVolumeHeader(header *rar5HeaderData, dataSize int64) error {
	if header.ExtraSize < 0 || header.ExtraSize > len(header.Data) {
		return fmt.Errorf("invalid RAR5 extra area size")
	}
	body := bytes.NewReader(header.Data[:len(header.Data)-header.ExtraSize])
	switch header.Type {
	case RAR5HeaderTypeMain:
		if r.volumeInfo != nil {
			return fmt.Errorf("duplicate RAR5 main header")
		}
		if dataSize != 0 || header.Flags&(RAR5HeaderFlagSplitBefore|RAR5HeaderFlagSplitAfter) != 0 {
			return fmt.Errorf("invalid RAR5 main header data flags")
		}
		flags, err := readVInt(body)
		if err != nil {
			return fmt.Errorf("read RAR5 archive flags: %w", err)
		}
		info := &rar5VolumeInfo{multi: flags&RAR5MainFlagVolume != 0}
		if flags&RAR5MainFlagVolumeNumber != 0 {
			if !info.multi {
				return fmt.Errorf("RAR5 volume number without volume flag")
			}
			info.number, err = readVInt(body)
			if err != nil {
				return fmt.Errorf("read RAR5 volume number: %w", err)
			}
			if info.number == 0 {
				return fmt.Errorf("RAR5 first volume must omit its number")
			}
		}
		if body.Len() != 0 {
			return fmt.Errorf("unexpected RAR5 main header fields")
		}
		r.volumeInfo = info
	case RAR5HeaderTypeFile, RAR5HeaderTypeService:
		if r.volumeInfo == nil {
			return fmt.Errorf("missing RAR5 main header before file or service")
		}
	case RAR5HeaderTypeEndOfArc:
		if r.volumeInfo == nil {
			return fmt.Errorf("missing RAR5 main header before archive end")
		}
		flags, err := readVInt(body)
		if err != nil {
			return fmt.Errorf("read RAR5 end flags: %w", err)
		}
		if flags & ^uint64(1) != 0 || body.Len() != 0 || dataSize != 0 ||
			header.Flags&(RAR5HeaderFlagSplitBefore|RAR5HeaderFlagSplitAfter) != 0 {
			return fmt.Errorf("invalid RAR5 end header")
		}
		r.volumeInfo.next = flags&1 != 0
	}
	return nil
}

// Return original descriptor positions in validated embedded order. Allocation
// is bounded by the admitted volume count, never by an untrusted header index.
func rar5VolumeOrder(infos []*rar5VolumeInfo) ([]int, error) {
	if len(infos) == 0 {
		return nil, fmt.Errorf("no RAR5 volume metadata")
	}
	order := make([]int, len(infos))
	seen := make([]bool, len(infos))
	for original, info := range infos {
		if info == nil {
			return nil, fmt.Errorf("missing RAR5 volume metadata")
		}
		if (len(infos) > 1 && !info.multi) || info.number >= uint64(len(infos)) {
			return nil, fmt.Errorf("incomplete RAR5 volume sequence at index %d", info.number)
		}
		if seen[info.number] {
			return nil, fmt.Errorf("duplicate RAR5 volume index %d", info.number)
		}
		if info.next != (info.number < uint64(len(infos)-1)) {
			return nil, fmt.Errorf("inconsistent RAR5 terminal marker at index %d", info.number)
		}
		seen[info.number] = true
		order[info.number] = original
	}
	return order, nil
}

// Build the flat byte map from the original, already validated descriptors.
// Re-running filename-position-based legacy geometry after reordering would
// assign the old last-volume size to a different posted file.
func buildRARBaseSegments(volumes []*types.Volume, order []int) ([]storage.NZBSegment, []storage.ArchiveVolumeInfo, error) {
	if len(order) == 0 || len(order) != len(volumes) {
		return nil, nil, fmt.Errorf("invalid RAR volume mapping")
	}
	var base []storage.NZBSegment
	var infos []storage.ArchiveVolumeInfo
	seen := make([]bool, len(volumes))
	var total int64
	for _, original := range order {
		if original < 0 || original >= len(volumes) || seen[original] {
			return nil, nil, fmt.Errorf("invalid RAR volume mapping")
		}
		seen[original] = true
		vol := volumes[original]
		if vol == nil || vol.Size <= 0 || vol.Size > math.MaxInt64-total {
			return nil, nil, fmt.Errorf("invalid RAR volume size")
		}
		var size int64
		for _, segment := range vol.Segments {
			if segment.Bytes <= 0 || segment.Bytes > vol.Size-size {
				return nil, nil, fmt.Errorf("invalid RAR volume segment geometry")
			}
			size += segment.Bytes
		}
		if size != vol.Size {
			return nil, nil, fmt.Errorf("incomplete RAR volume segment geometry")
		}
		start := len(base)
		base = append(base, vol.Segments...)
		infos = append(infos, storage.ArchiveVolumeInfo{Name: vol.Name, Size: vol.Size, SegmentStart: start, SegmentEnd: len(base)})
		total += vol.Size
	}
	return base, infos, nil
}
