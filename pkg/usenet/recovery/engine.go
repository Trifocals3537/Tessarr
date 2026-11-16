package recovery

import (
	"bytes"
	"context"
	"crypto/md5" // Required by the PAR2 format.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Tensai75/nzbparser"
	"github.com/Trifocals3537/tessarr/internal/nntp"
	"github.com/Trifocals3537/tessarr/internal/par2"
	"github.com/Trifocals3537/tessarr/pkg/storage"
)

const MaxSourceBytes = 16 << 20
const MaxArticleBytes = 4 << 20
const MaxReadBytes int64 = 64 << 30
const MaxReleaseBytes int64 = 32 << 30
const MaxBufferBytes int64 = 128 << 20
const DownloadBytesPerSecond int64 = 8 << 20
const JobTimeout = 6 * time.Hour

var ErrUnsupported = errors.New("only existing plain-file NZBs with an intact source map can be repaired")
var ErrStale = errors.New("the NZB or file changed; recovery was stopped")
var ErrLimit = errors.New("recovery exceeds this version's resource limits")
var ErrParity = errors.New("no usable or sufficient PAR2 recovery data is available")
var ErrTemporary = errors.New("a provider is temporarily unavailable; retry when it recovers")
var ErrSourceRequired = errors.New("select the original NZB to provide PAR2 recovery data")

type Target struct {
	ID         string
	Name       string
	File       storage.NZBFile
	Files      []storage.NZBFile
	Source     []byte
	Generation string
}

// Host keeps recovery independent from the live manager/server. Downloads use
// the shared provider pool and must account for every decoded byte, including
// failed/retried transfers, through consume.
type Host interface {
	Snapshot(context.Context, string, string) (Target, error)
	Download(context.Context, string, *storage.NZBArticleGeometry, func(int) error) ([]byte, *nntp.YencMetadata, error)
	WaitIdle(context.Context) error
	Published(Target)
}

func Eligible(f *storage.NZBFile) error {
	if f == nil || f.IsDeleted || f.FileType != storage.NZBFileTypeMedia || f.InternalPath != "" || f.IsEncrypted || len(f.EncryptionKey) != 0 || len(f.EncryptionIV) != 0 || f.StartOffset != 0 || f.Size <= 0 || f.Size > MaxReleaseBytes || len(f.Segments) == 0 || len(f.Segments) > 100000 {
		return ErrUnsupported
	}
	var end int64
	seen := make(map[string]bool, len(f.Segments))
	for _, s := range f.Segments {
		if s.MessageID == "" || len(s.MessageID) > 1024 || seen[s.MessageID] || s.SegmentDataStart != 0 || s.StartOffset != end || s.Bytes <= 0 || s.Bytes > MaxArticleBytes || s.Bytes > f.Size-end || s.EndOffset != end+s.Bytes-1 {
			return ErrUnsupported
		}
		seen[s.MessageID] = true
		if s.Source != nil && (!s.Source.Valid() || s.Source.Size != f.Size || s.Source.Offset != s.StartOffset || s.Source.Bytes != s.Bytes) {
			return ErrUnsupported
		}
		end += s.Bytes
	}
	if end != f.Size {
		return ErrUnsupported
	}
	return nil
}

// Generation excludes mutable NZB history/status and includes the complete
// immutable file map. Reused NZB IDs/names cannot inherit an earlier repair.
func Generation(f storage.NZBFile) string {
	b, _ := json.Marshal(f)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type article struct {
	id       string
	geometry storage.NZBArticleGeometry
}
type remoteFile struct {
	name       string
	size       int64
	articles   []article
	cache      *articleCache
	generation string
	store      *Store
}
type articleCache struct {
	ctx     context.Context
	host    Host
	consume func(int) error
	id      string
	data    []byte
	meta    *nntp.YencMetadata
}

func (c *articleCache) get(id string, g *storage.NZBArticleGeometry) ([]byte, *nntp.YencMetadata, error) {
	if err := c.ctx.Err(); err != nil {
		return nil, nil, err
	}
	if c.id == id && c.meta != nil && (g == nil || g.Matches(c.meta.Size, c.meta.Begin, c.meta.End, c.meta.Part, c.meta.Total, int64(len(c.data)))) {
		return c.data, c.meta, nil
	}
	if err := c.host.WaitIdle(c.ctx); err != nil {
		return nil, nil, err
	}
	// Drop the old buffer before allocating another bounded article.
	c.id = ""
	c.data = nil
	c.meta = nil
	b, m, err := c.host.Download(c.ctx, id, g, c.consume)
	if err != nil {
		return nil, nil, err
	}
	if len(b) == 0 || len(b) > MaxArticleBytes || m == nil || m.Size <= 0 || m.Size > MaxReleaseBytes || m.Begin < 1 || m.End-m.Begin+1 != int64(len(b)) || m.End > m.Size {
		return nil, nil, ErrUnsupported
	}
	if g != nil && !g.Matches(m.Size, m.Begin, m.End, m.Part, m.Total, int64(len(b))) {
		return nil, nil, ErrStale
	}
	c.id = id
	c.data = b
	c.meta = m
	return b, m, nil
}

func (f *remoteFile) ReadAt(p []byte, off int64) (int, error) {
	if f.store != nil && f.generation != "" {
		return f.store.Overlay(f.cache.ctx, f.generation, f.size, p, off, f.readAt)
	}
	return f.readAt(f.cache.ctx, p, off)
}

func (f *remoteFile) readAt(ctx context.Context, p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, ErrUnsupported
	}
	if off >= f.size {
		return 0, io.EOF
	}
	want := min(int64(len(p)), f.size-off)
	done := int64(0)
	i := sort.Search(len(f.articles), func(i int) bool { return f.articles[i].geometry.Offset+f.articles[i].geometry.Bytes > off })
	for done < want && i < len(f.articles) {
		if err := ctx.Err(); err != nil {
			return int(done), err
		}
		a := f.articles[i]
		b, _, err := f.cache.get(a.id, &a.geometry)
		if err != nil {
			return int(done), err
		}
		start := off + done - a.geometry.Offset
		if start < 0 || start >= int64(len(b)) {
			return int(done), ErrUnsupported
		}
		n := min(want-done, int64(len(b))-start)
		copy(p[done:done+n], b[start:start+n])
		done += n
		i++
	}
	if done < int64(len(p)) {
		return int(done), io.EOF
	}
	return int(done), nil
}

func rawIDs(raw nzbparser.NzbFile) ([]string, error) {
	segments := append(nzbparser.NzbSegments(nil), raw.Segments...)
	if len(segments) == 0 || len(segments) > 100000 {
		return nil, ErrUnsupported
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].Number < segments[j].Number })
	base := segments[0].Number
	if base != 0 && base != 1 {
		return nil, ErrUnsupported
	}
	ids := make([]string, len(segments))
	seen := make(map[string]bool, len(segments))
	for i, s := range segments {
		if strings.IndexFunc(s.Id, func(r rune) bool { return r < 33 || r > 126 || r == '<' || r == '>' }) >= 0 {
			return nil, ErrUnsupported
		}
		if s.Number != base+i || s.Id == "" || len(s.Id) > 1024 || seen[s.Id] {
			return nil, ErrUnsupported
		}
		ids[i] = s.Id
		seen[s.Id] = true
	}
	return ids, nil
}

func sameIDs(ids []string, file storage.NZBFile) bool {
	if len(ids) != len(file.Segments) {
		return false
	}
	for i, id := range ids {
		if id != file.Segments[i].MessageID {
			return false
		}
	}
	return true
}

func ValidateSource(source []byte, file storage.NZBFile) error {
	if len(source) == 0 {
		return ErrSourceRequired
	}
	if len(source) > MaxSourceBytes {
		return ErrLimit
	}
	raw, err := nzbparser.Parse(bytes.NewReader(source))
	if err != nil {
		return ErrUnsupported
	}
	if len(raw.Files) == 0 || len(raw.Files) > 512 {
		return ErrLimit
	}
	matched, parity, total := 0, 0, 0
	for _, f := range raw.Files {
		ids, e := rawIDs(f)
		if e != nil {
			return e
		}
		total += len(ids)
		if total > 100000 {
			return ErrLimit
		}
		if sameIDs(ids, file) {
			matched++
		}
		if strings.HasSuffix(strings.ToLower(f.Filename), ".par2") {
			parity++
		}
	}
	if matched != 1 {
		return ErrStale
	}
	if parity == 0 {
		return ErrParity
	}
	if parity > 64 {
		return ErrLimit
	}
	return nil
}

func knownFile(raw nzbparser.NzbFile, file storage.NZBFile, cache *articleCache, store *Store) (*remoteFile, error) {
	if err := Eligible(&file); err != nil {
		return nil, err
	}
	ids, err := rawIDs(raw)
	if err != nil || !sameIDs(ids, file) {
		return nil, ErrStale
	}
	f := &remoteFile{name: raw.Filename, size: file.Size, cache: cache, generation: Generation(file), store: store}
	for i, s := range file.Segments {
		g := storage.NZBArticleGeometry{Size: file.Size, Offset: s.StartOffset, Bytes: s.Bytes, Part: int64(i + 1), Total: int64(len(ids))}
		f.articles = append(f.articles, article{ids[i], g})
	}
	return f, nil
}

// Newly opened recovery-set members use a verified yEnc part to establish the
// same fixed-part layout as Tessarr's import parser. NZB encoded byte estimates
// and untrusted subject counters never become decoded offsets.
func discoverFile(raw nzbparser.NzbFile, cache *articleCache) (*remoteFile, error) {
	ids, err := rawIDs(raw)
	if err != nil {
		return nil, err
	}
	for i := 0; i < min(len(ids), 16); i++ {
		_, m, err := cache.get(ids[i], nil)
		if err != nil {
			if nntp.IsArticleNotFoundError(err) {
				continue
			}
			return nil, err
		}
		part := m.End - m.Begin + 1
		if i == len(ids)-1 && i > 0 {
			if (m.Begin-1)%int64(i) != 0 {
				return nil, ErrUnsupported
			}
			part = (m.Begin - 1) / int64(i)
		}
		if part <= 0 || part > MaxArticleBytes || (m.Size-1)/part+1 != int64(len(ids)) || m.Begin != int64(i)*part+1 || m.End != min(m.Size, int64(i+1)*part) || (m.Part != 0 && m.Part != int64(i+1)) || (m.Total != 0 && m.Total != int64(len(ids))) {
			return nil, ErrUnsupported
		}
		f := &remoteFile{name: raw.Filename, size: m.Size, cache: cache}
		for j, id := range ids {
			offset := int64(j) * part
			f.articles = append(f.articles, article{id, storage.NZBArticleGeometry{Size: m.Size, Offset: offset, Bytes: min(part, m.Size-offset), Part: int64(j + 1), Total: int64(len(ids))}})
		}
		return f, nil
	}
	return nil, ErrParity
}

type progress func(stage string, readBytes int64) error

func Run(ctx context.Context, target Target, host Host, store *Store, alreadyRead int64, report progress) error {
	if report == nil {
		report = func(string, int64) error { return nil }
	}
	if err := Eligible(&target.File); err != nil {
		return err
	}
	if len(target.Source) == 0 || len(target.Source) > MaxSourceBytes || target.Generation != Generation(target.File) {
		return ErrStale
	}
	raw, err := nzbparser.Parse(bytes.NewReader(target.Source))
	if err != nil {
		return ErrUnsupported
	}
	if len(raw.Files) == 0 || len(raw.Files) > 512 {
		return ErrLimit
	}
	var sourceTarget *nzbparser.NzbFile
	totalSegments := 0
	for i := range raw.Files {
		ids, e := rawIDs(raw.Files[i])
		if e != nil {
			return e
		}
		totalSegments += len(ids)
		if totalSegments > 100000 {
			return ErrLimit
		}
		if sameIDs(ids, target.File) {
			if sourceTarget != nil {
				return ErrUnsupported
			}
			sourceTarget = &raw.Files[i]
		}
	}
	if sourceTarget == nil {
		return ErrStale
	}
	readBytes := alreadyRead
	start := time.Now()
	sessionBytes := int64(0)
	stage := "Reading recovery information"
	consume := func(n int) error {
		if n < 0 || int64(n) > MaxReadBytes-readBytes {
			return ErrLimit
		}
		readBytes += int64(n)
		sessionBytes += int64(n)
		if err := report(stage, readBytes); err != nil {
			return err
		}
		// Pace decoded bytes, including failed and retried article bodies.
		wait := time.Duration(float64(sessionBytes)/float64(DownloadBytesPerSecond)*float64(time.Second)) - time.Since(start)
		if wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
		return ctx.Err()
	}
	cache := &articleCache{ctx: ctx, host: host, consume: consume}
	targetRemote, err := knownFile(*sourceTarget, target.File, cache, store)
	if err != nil {
		return err
	}
	var parityRaw []nzbparser.NzbFile
	for _, f := range raw.Files {
		if strings.HasSuffix(strings.ToLower(f.Filename), ".par2") {
			parityRaw = append(parityRaw, f)
		}
	}
	if len(parityRaw) == 0 {
		return ErrParity
	}
	if len(parityRaw) > 64 {
		return ErrLimit
	}
	sort.Slice(parityRaw, func(i, j int) bool { return parityRaw[i].Filename < parityRaw[j].Filename })
	var sources []par2.Source
	for _, f := range parityRaw {
		remote, e := discoverFile(f, cache)
		if e != nil {
			if errors.Is(e, ErrParity) || nntp.IsArticleNotFoundError(e) {
				continue
			}
			return e
		}
		sources = append(sources, par2.Source{Name: f.Filename, Size: remote.size, Reader: remote, Missing: nntp.IsArticleNotFoundError})
	}
	idx, err := par2.ReadIndexNames(ctx, sources, []string{sourceTarget.Filename, target.File.Name}, target.File.Size)
	if err != nil {
		if errors.Is(err, par2.ErrLimit) {
			return errors.Join(ErrLimit, err)
		}
		return errors.Join(ErrParity, err)
	}
	var files []*remoteFile
	targetIndex := -1
	releaseBytes := int64(0)
	for i, f := range idx.Files {
		if f.Size > MaxReleaseBytes-releaseBytes {
			return ErrLimit
		}
		releaseBytes += f.Size
		if (f.Name == sourceTarget.Filename || f.Name == target.File.Name) && f.Size == target.File.Size {
			if targetIndex >= 0 {
				return ErrParity
			}
			targetIndex = i
			files = append(files, targetRemote)
			continue
		}
		var matches []nzbparser.NzbFile
		for _, r := range raw.Files {
			if r.Filename == f.Name {
				matches = append(matches, r)
			}
		}
		if len(matches) != 1 {
			return ErrParity
		}
		var remote *remoteFile
		for _, known := range target.Files {
			ids, _ := rawIDs(matches[0])
			if sameIDs(ids, known) && Eligible(&known) == nil {
				remote, err = knownFile(matches[0], known, cache, store)
				break
			}
		}
		if remote == nil {
			remote, err = discoverFile(matches[0], cache)
		}
		if err != nil {
			return err
		}
		if remote.size != f.Size {
			return ErrStale
		}
		files = append(files, remote)
	}
	if targetIndex < 0 {
		return ErrParity
	}
	// The reserve covers source/metadata parsing, bounded article/input buffers,
	// and bookkeeping. No allocation grows with release byte size.
	rowLimit := min(par2.MaxRecoveryRows, int((MaxBufferBytes-(64<<20))/(2*int64(idx.SliceSize))))
	if rowLimit < 1 {
		return ErrLimit
	}
	var rows [][]byte
	var exponents []uint32
	seen := make(map[uint32]bool)
	for _, ref := range idx.Recovery {
		if seen[ref.Exponent] {
			continue
		}
		b, e := par2.ReadRecovery(ctx, sources[ref.Source], ref)
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !errors.Is(e, par2.ErrIntegrity) && !nntp.IsArticleNotFoundError(e) {
				return e
			}
			continue
		}
		seen[ref.Exponent] = true
		rows = append(rows, b)
		exponents = append(exponents, ref.Exponent)
		if len(rows) == rowLimit {
			break
		}
	}
	if len(rows) == 0 {
		return ErrParity
	}
	solver, err := par2.NewSolver(idx.SliceSize, exponents, rows, host.WaitIdle)
	if err != nil {
		return err
	}
	stage = "Checking and recovering data"
	if err := report(stage, readBytes); err != nil {
		return err
	}
	buffer := make([]byte, idx.SliceSize)
	global := 0
	targetStart := 0
	targetMissing := make(map[int]int64)
	for fi, f := range idx.Files {
		if fi == targetIndex {
			targetStart = global
		}
		for off := int64(0); off < f.Size; off += int64(idx.SliceSize) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := host.WaitIdle(ctx); err != nil {
				return err
			}
			clear(buffer)
			want := min(int64(idx.SliceSize), f.Size-off)
			n, e := files[fi].ReadAt(buffer[:want], off)
			missing := nntp.IsArticleNotFoundError(e)
			if e != nil && !missing {
				return e
			}
			if !missing && (int64(n) != want || !par2.VerifySlice(buffer, f.Checks[off/int64(idx.SliceSize)])) {
				missing = true
			}
			if missing {
				if fi == targetIndex {
					targetMissing[global] = off
					if int64(len(targetMissing))*int64(idx.SliceSize) > MaxPatchBytes {
						return ErrLimit
					}
				}
				err = solver.Missing(global)
			} else {
				err = solver.Fold(ctx, global, buffer)
			}
			if err != nil {
				return errors.Join(ErrParity, err)
			}
			global++
		}
	}
	recovered, err := solver.Solve(ctx)
	if err != nil {
		return errors.Join(ErrParity, err)
	}
	global = 0
	for _, f := range idx.Files {
		for _, check := range f.Checks {
			if b, ok := recovered[global]; ok && !par2.VerifySlice(b, check) {
				return ErrParity
			}
			global++
		}
	}
	var blocks []Block
	for n, off := range targetMissing {
		blocks = append(blocks, Block{Offset: off, Data: recovered[n][:min(int64(idx.SliceSize), target.File.Size-off)]})
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].Offset < blocks[j].Offset })
	stage = "Verifying the complete file"
	if err := report(stage, readBytes); err != nil {
		return err
	}
	// Independently verify the complete unpadded target MD5 in file order. The
	// overlay avoids fetching missing bytes again and includes any older repair.
	hash := md5.New()
	clear(buffer)
	for off := int64(0); off < target.File.Size; off += int64(idx.SliceSize) {
		want := min(int64(idx.SliceSize), target.File.Size-off)
		if b, ok := recovered[targetStart+int(off/int64(idx.SliceSize))]; ok {
			copy(buffer[:want], b[:want])
		} else {
			n, e := targetRemote.ReadAt(buffer[:want], off)
			if e != nil || int64(n) != want {
				if e != nil {
					return e
				}
				return io.ErrUnexpectedEOF
			}
		}
		_, _ = hash.Write(buffer[:want])
	}
	if !bytes.Equal(hash.Sum(nil), idx.Files[targetIndex].MD5[:]) {
		return ErrParity
	}
	validate := func() error {
		current, e := host.Snapshot(ctx, target.ID, target.File.Name)
		if e != nil {
			return e
		}
		if current.Generation != target.Generation || sha256.Sum256(current.Source) != sha256.Sum256(target.Source) {
			return ErrStale
		}
		return ctx.Err()
	}
	if err := validate(); err != nil {
		return err
	}
	if len(blocks) > 0 {
		stage = "Saving verified recovery"
		if err := report(stage, readBytes); err != nil {
			return err
		}
		if err := store.Publish(ctx, target.Generation, target.File.Size, hex.EncodeToString(idx.Set[:]), hex.EncodeToString(idx.Files[targetIndex].MD5[:]), blocks, validate, Identity{NZBID: target.ID, File: target.File.Name}); err != nil {
			return err
		}
	}
	host.Published(target)
	return nil
}
