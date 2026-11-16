package par2

import (
	"bytes"
	"context"
	"crypto/md5" // PAR2 format checksums, not authentication.
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	MaxSliceBytes    = 8 << 20
	MaxRecoveryRows  = 16
	MaxInputSlices   = 32768
	MaxFiles         = 256
	maxMetadataBytes = 8 << 20
	maxPacketCount   = 65536
	maxResyncBytes   = 32 << 20
)

var magic = [8]byte{'P', 'A', 'R', '2', 0, 'P', 'K', 'T'}
var mainType = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0, 'M', 'a', 'i', 'n'}
var descType = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0, 'F', 'i', 'l', 'e', 'D', 'e', 's', 'c'}
var checkType = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0, 'I', 'F', 'S', 'C'}
var recoveryType = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0, 'R', 'e', 'c', 'v', 'S', 'l', 'i', 'c'}

type Source struct {
	Name    string
	Size    int64
	Reader  io.ReaderAt
	Missing func(error) bool
	// SkipMissing returns the end of the confirmed missing article at offset.
	// It avoids repeatedly requesting the same absent article during scanning.
	SkipMissing func(int64) int64
}

var ErrIntegrity = errors.New("PAR2 payload failed integrity verification")
var ErrLimit = errors.New("PAR2 dimensions exceed recovery limits")

func skippable(source Source, err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || (source.Missing != nil && source.Missing(err))
}

// resync looks for the next aligned packet magic with one bounded window.
// The shared search budget includes skipped unavailable spans as well as data.
func resync(ctx context.Context, source Source, off int64, budget *int64) (int64, bool, error) {
	buffer := make([]byte, 64<<10+7)
	for off <= source.Size-64 {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		if *budget <= 0 {
			return 0, false, fmt.Errorf("%w: packet resynchronization", ErrLimit)
		}
		want := min(int64(len(buffer)), source.Size-off, *budget+7)
		n, err := source.Reader.ReadAt(buffer[:want], off)
		if n < 0 || int64(n) > want {
			return 0, false, errors.New("invalid PAR2 scan read count")
		}
		for start := 0; start+8 <= n; {
			i := bytes.Index(buffer[start:n], magic[:])
			if i < 0 {
				break
			}
			i += start
			if (off+int64(i))%4 == 0 {
				*budget -= int64(i)
				return off + int64(i), true, nil
			}
			start = i + 1
		}
		advance := max(int64(n)-7, int64(1))
		if err != nil {
			if !skippable(source, err) {
				return 0, false, err
			}
			if source.Missing != nil && source.Missing(err) {
				if source.SkipMissing != nil {
					advance = max(advance, source.SkipMissing(off+int64(n))-off)
				} else {
					advance = max(advance, int64(4096))
				}
			} else {
				return 0, false, nil
			}
		}
		if advance > *budget {
			return 0, false, fmt.Errorf("%w: packet resynchronization", ErrLimit)
		}
		*budget -= advance
		off += advance
	}
	return 0, false, nil
}

type Check struct {
	MD5   [16]byte
	CRC32 uint32
}
type File struct {
	ID, MD5, HeadMD5 [16]byte
	Name             string
	Size             int64
	Checks           []Check
}
type Recovery struct {
	Source    int
	Offset    int64
	Exponent  uint32
	Hash, Set [16]byte
	Size      int
}
type Index struct {
	Set       [16]byte
	SliceSize int
	Files     []File
	Recovery  []Recovery
}
type builder struct {
	main     []byte
	files    map[[16]byte]File
	checks   map[[16]byte][]Check
	refs     []Recovery
	conflict bool
}

// ReadIndex accepts one unambiguous recovery set for this exact original file.
// Metadata and packet counts are bounded before allocation. References to
// recovery payloads are lazy; unrelated recovery rows are never downloaded.
func ReadIndex(ctx context.Context, sources []Source, target string, size int64) (*Index, error) {
	return ReadIndexNames(ctx, sources, []string{target}, size)
}

func ReadIndexNames(ctx context.Context, sources []Source, targets []string, size int64) (*Index, error) {
	if len(sources) > 64 {
		return nil, ErrLimit
	}
	if len(sources) == 0 {
		return nil, errors.New("no usable PAR2 sources")
	}
	sets := make(map[[16]byte]*builder)
	seenMetadata := make(map[[16]byte]bool)
	metadata, packets := 0, 0
	resyncBudget := int64(maxResyncBytes)
	for si, source := range sources {
		for off := int64(0); off <= source.Size-64; {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			packets++
			if packets > maxPacketCount {
				return nil, fmt.Errorf("%w: packet count", ErrLimit)
			}
			var header [64]byte
			n, readErr := source.Reader.ReadAt(header[:], off)
			if readErr != nil && !skippable(source, readErr) {
				return nil, readErr
			}
			length := binary.LittleEndian.Uint64(header[8:16])
			if readErr != nil || n != len(header) || [8]byte(header[:8]) != magic || length < 64 || length%4 != 0 || length > uint64(MaxSliceBytes+68) || length > uint64(source.Size-off) {
				start := off + 4
				if readErr != nil && source.Missing != nil && source.Missing(readErr) && source.SkipMissing != nil {
					start = max(start, source.SkipMissing(off+int64(n)))
				}
				// Charge a skipped missing header/article before the scan begins.
				if start-off > resyncBudget {
					return nil, ErrLimit
				}
				resyncBudget -= start - off
				next, found, err := resync(ctx, source, start, &resyncBudget)
				if err != nil {
					return nil, err
				}
				if !found {
					break
				}
				off = next
				continue
			}
			set, kind := [16]byte(header[32:48]), [16]byte(header[48:64])
			b := sets[set]
			if b == nil {
				if len(sets) >= 8 {
					return nil, fmt.Errorf("%w: recovery sets", ErrLimit)
				}
				b = &builder{files: make(map[[16]byte]File), checks: make(map[[16]byte][]Check)}
				sets[set] = b
			}
			if kind == recoveryType {
				if length >= 72 && len(b.refs) < 256 {
					var exponent [4]byte
					if _, err := source.Reader.ReadAt(exponent[:], off+64); err == nil {
						b.refs = append(b.refs, Recovery{Source: si, Offset: off + 68, Exponent: binary.LittleEndian.Uint32(exponent[:]), Hash: [16]byte(header[16:32]), Set: set, Size: int(length) - 68})
					} else if !skippable(source, err) {
						return nil, err
					}
				}
			} else if kind == mainType || kind == descType || kind == checkType {
				hash := [16]byte(header[16:32])
				if seenMetadata[hash] {
					off += int64(length)
					continue
				}
				bodySize := int(length) - 64
				metadata += bodySize
				if metadata > maxMetadataBytes {
					return nil, fmt.Errorf("%w: metadata", ErrLimit)
				}
				body := make([]byte, bodySize)
				if _, err := source.Reader.ReadAt(body, off+64); err == nil {
					h := md5.New()
					_, _ = h.Write(header[32:])
					_, _ = h.Write(body)
					if bytes.Equal(h.Sum(nil), header[16:32]) {
						seenMetadata[hash] = true
						b.absorb(kind, set, body)
					}
				} else if !skippable(source, err) {
					return nil, err
				}
			}
			off += int64(length)
		}
	}
	var selected *Index
	var limitErr error
	for set, b := range sets {
		idx, err := b.index(set)
		if err != nil {
			if errors.Is(err, ErrLimit) {
				limitErr = err
			}
			continue
		}
		match := false
		for _, f := range idx.Files {
			if slices.Contains(targets, f.Name) && f.Size == size {
				match = true
			}
		}
		if !match {
			continue
		}
		if selected != nil {
			return nil, errors.New("ambiguous PAR2 recovery set")
		}
		selected = idx
	}
	if selected == nil {
		if limitErr != nil {
			return nil, limitErr
		}
		return nil, errors.New("no complete PAR2 metadata matches this file")
	}
	return selected, nil
}

func (b *builder) absorb(kind, set [16]byte, body []byte) {
	switch kind {
	case mainType:
		if len(body) < 28 || (len(body)-12)%16 != 0 || md5.Sum(body) != set {
			return
		}
		if b.main != nil && !bytes.Equal(b.main, body) {
			b.conflict = true
		}
		b.main = body
	case descType:
		if len(body) < 60 || len(body) > 56+4096 {
			return
		}
		name := strings.TrimRight(string(body[56:]), "\x00")
		if name == "" || !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
			return
		}
		h := md5.New()
		_, _ = h.Write(body[32:56])
		_, _ = h.Write([]byte(name))
		id := [16]byte(body[:16])
		if !bytes.Equal(h.Sum(nil), id[:]) {
			return
		}
		n := binary.LittleEndian.Uint64(body[48:56])
		if n == 0 || n > 256<<30 {
			return
		}
		f := File{ID: id, MD5: [16]byte(body[16:32]), HeadMD5: [16]byte(body[32:48]), Name: name, Size: int64(n)}
		if old, ok := b.files[id]; ok && (old.Name != f.Name || old.Size != f.Size || old.MD5 != f.MD5) {
			b.conflict = true
		}
		if len(b.files) < MaxFiles {
			b.files[id] = f
		}
	case checkType:
		if len(body) < 36 || (len(body)-16)%20 != 0 || (len(body)-16)/20 > MaxInputSlices {
			return
		}
		id := [16]byte(body[:16])
		checks := make([]Check, (len(body)-16)/20)
		for i := range checks {
			p := body[16+i*20:]
			checks[i] = Check{[16]byte(p[:16]), binary.LittleEndian.Uint32(p[16:20])}
		}
		if old, ok := b.checks[id]; ok && !slices.Equal(old, checks) {
			b.conflict = true
		}
		if len(b.checks) < MaxFiles {
			b.checks[id] = checks
		}
	}
}

func (b *builder) index(set [16]byte) (*Index, error) {
	if b.conflict || len(b.main) < 28 {
		return nil, errors.New("incomplete or conflicting recovery metadata")
	}
	size := binary.LittleEndian.Uint64(b.main[:8])
	count := binary.LittleEndian.Uint32(b.main[8:12])
	if size > MaxSliceBytes || count > MaxFiles {
		return nil, ErrLimit
	}
	if size == 0 || size%4 != 0 || count == 0 || uint64(count) > uint64((len(b.main)-12)/16) {
		return nil, errors.New("unsupported PAR2 dimensions")
	}
	idx := &Index{Set: set, SliceSize: int(size)}
	seen := make(map[[16]byte]bool)
	blocks := int64(0)
	for i := range int(count) {
		id := [16]byte(b.main[12+i*16 : 12+(i+1)*16])
		f, ok := b.files[id]
		if !ok || seen[id] {
			return nil, errors.New("incomplete or duplicate recovery member")
		}
		seen[id] = true
		n := (f.Size-1)/int64(size) + 1
		blocks += n
		if blocks > MaxInputSlices {
			return nil, ErrLimit
		}
		if int64(len(b.checks[id])) != n {
			return nil, errors.New("incomplete or oversized slice checksums")
		}
		f.Checks = b.checks[id]
		idx.Files = append(idx.Files, f)
	}
	for _, ref := range b.refs {
		if ref.Size == idx.SliceSize && ref.Set == set {
			idx.Recovery = append(idx.Recovery, ref)
		}
	}
	if len(idx.Recovery) == 0 {
		return nil, errors.New("no recovery slices available")
	}
	return idx, nil
}

func ReadRecovery(ctx context.Context, source Source, ref Recovery) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ref.Size <= 0 || ref.Size > MaxSliceBytes || ref.Offset < 68 || ref.Offset > source.Size-int64(ref.Size) {
		return nil, errors.New("invalid recovery payload bounds")
	}
	b := make([]byte, ref.Size)
	if _, err := source.Reader.ReadAt(b, ref.Offset); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errors.Join(ErrIntegrity, err)
		}
		return nil, err
	}
	h := md5.New()
	_, _ = h.Write(ref.Set[:])
	_, _ = h.Write(recoveryType[:])
	var exp [4]byte
	binary.LittleEndian.PutUint32(exp[:], ref.Exponent)
	_, _ = h.Write(exp[:])
	_, _ = h.Write(b)
	if !bytes.Equal(h.Sum(nil), ref.Hash[:]) {
		return nil, ErrIntegrity
	}
	return b, nil
}

func VerifySlice(data []byte, check Check) bool {
	return md5.Sum(data) == check.MD5 && crc32.ChecksumIEEE(data) == check.CRC32
}

func (f File) String() string { return fmt.Sprintf("%s (%d bytes)", f.Name, f.Size) }
