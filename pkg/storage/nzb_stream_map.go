package storage

import (
	"crypto/aes"
	"errors"
	"fmt"
	"math"
)

// ErrInvalidNZBMap identifies a local metadata defect, not an upstream
// article-availability observation.
var ErrInvalidNZBMap = errors.New("invalid NZB segment map")

// NZBStoredStreamSize includes the last complete AES-CBC cipher block.
// The logical size advertised to readers excludes this padding.
func NZBStoredStreamSize(size int64, encrypted bool) (int64, error) {
	if size < 0 {
		return 0, fmt.Errorf("%w: negative stored file size", ErrInvalidNZBMap)
	}
	if encrypted && size%aes.BlockSize != 0 {
		padding := int64(aes.BlockSize) - size%aes.BlockSize
		if size > math.MaxInt64-padding {
			return 0, fmt.Errorf("%w: stored file size overflow", ErrInvalidNZBMap)
		}
		size += padding
	}
	return size, nil
}

// ValidateNZBStreamMap validates a stored logical file before it can serve
// bytes. Older maps may lack both offsets and source geometry; cumulative
// offsets are safe only when those offsets are wholly unspecified and the
// positive segment lengths cover exactly the advertised stored size.
// An explicit but inconsistent map must never be silently re-laid out.
// New imports require exact offsets and immutable source geometry.
func ValidateNZBStreamMap(file *NZBFile, requireSource bool) error {
	if file == nil || file.Size <= 0 || len(file.Segments) == 0 {
		return fmt.Errorf("%w: empty file or segments", ErrInvalidNZBMap)
	}
	size, err := NZBStoredStreamSize(file.Size, file.IsEncrypted)
	if err != nil {
		return err
	}
	implicit := !requireSource
	if implicit {
		for _, seg := range file.Segments {
			if seg.StartOffset != 0 || seg.EndOffset != 0 || seg.Source != nil {
				implicit = false
				break
			}
		}
	}
	var offset int64
	for _, seg := range file.Segments {
		if seg.MessageID == "" || seg.Bytes <= 0 || seg.Bytes > size-offset ||
			seg.SegmentDataStart < 0 || seg.SegmentDataStart > math.MaxInt64-seg.Bytes {
			return fmt.Errorf("%w: invalid segment length or source slice", ErrInvalidNZBMap)
		}
		if !implicit && (seg.StartOffset != offset || seg.EndOffset != offset+seg.Bytes-1) {
			return fmt.Errorf("%w: noncontiguous logical offsets", ErrInvalidNZBMap)
		}
		if requireSource || seg.Source != nil {
			if !seg.Source.Valid() || seg.SegmentDataStart > seg.Source.Bytes || seg.Bytes > seg.Source.Bytes-seg.SegmentDataStart {
				return fmt.Errorf("%w: invalid source article geometry", ErrInvalidNZBMap)
			}
		}
		offset += seg.Bytes
	}
	if offset != size {
		return fmt.Errorf("%w: incomplete logical coverage", ErrInvalidNZBMap)
	}
	return nil
}
