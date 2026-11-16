// Package recovery implements manually requested, bounded PAR2 recovery.
package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Trifocals3537/tessarr/internal/safepath"
	"github.com/google/uuid"
)

const MaxPatchBytes int64 = 64 << 20
const MaxStoreBytes int64 = 512 << 20
const maxBlocks = 64
const maxHeaderBytes = 64 << 10

var objectMagic = [8]byte{'T', 'S', 'P', 'A', 'R', '2', '\r', '\n'}

type Block struct {
	Offset int64
	Data   []byte
}
type Identity struct{ NZBID, File string }
type blockRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}
type bundleHeader struct {
	Version    int          `json:"version"`
	Generation string       `json:"generation"`
	Target     string       `json:"target"`
	NZBID      string       `json:"nzb_id"`
	Size       int64        `json:"size"`
	Set        string       `json:"set"`
	MD5        string       `json:"md5"`
	Blocks     []blockRange `json:"blocks"`
}
type pointer struct {
	Object string `json:"object"`
	Target string `json:"target"`
	NZBID  string `json:"nzb_id"`
}
type view struct {
	file    *os.File
	header  bundleHeader
	starts  []int64
	refs    int
	retired bool
	object  string
}

// Store publishes an immutable content-addressed object before replacing its
// tiny generation pointer. Neither a crash nor a concurrent reader can observe
// a partially written patch. A generation is a digest of the complete file map.
type Store struct {
	mu       sync.Mutex
	root     *os.Root
	path     string
	pointers map[string]string
	targets  map[string]bool
	keys     map[string]string
	ids      map[string]string
	views    map[string]*view
	closed   bool
	// Test hooks are unset in production and model process/power-loss boundaries.
	beforePointer func() error
	afterPointer  func() error
}

func digestString(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && s == strings.ToLower(s)
}

func objectName(name, generation string) bool {
	return len(name) == 64+1+64+6 && strings.HasPrefix(name, generation+"-") && strings.HasSuffix(name, ".patch") && digestString(name[65:129], 64)
}

func OpenStore(path string) (*Store, error) {
	root, absolute, err := safepath.EnsureOpenRoot(path, 0o700)
	if err != nil {
		return nil, err
	}
	return loadStore(root, absolute)
}

// Takes ownership of root. The service acquires its lock through this same
// handle before loading or collecting any stored state.
func loadStore(root *os.Root, absolute string) (*Store, error) {
	s := &Store{root: root, path: absolute, pointers: make(map[string]string), targets: make(map[string]bool), keys: make(map[string]string), ids: make(map[string]string), views: make(map[string]*view)}
	if err := s.load(); err != nil {
		_ = root.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) entries() ([]os.DirEntry, error) {
	f, err := s.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(20001)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > 20000 {
		return nil, ErrLimit
	}
	return entries, nil
}

// Caller holds the exclusive service lock during startup; only our canonical
// object/temporary names are eligible for orphan cleanup.
func (s *Store) load() error {
	entries, err := s.entries()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || !digestString(strings.TrimSuffix(name, ".json"), 64) {
			continue
		}
		var p pointer
		if err := s.readJSON(name, &p, 1024); err != nil {
			return errors.New("invalid recovery pointer")
		}
		gen := strings.TrimSuffix(name, ".json")
		if !objectName(p.Object, gen) || !digestString(p.Target, 64) || safepath.ValidateIdentifier(p.NZBID) != nil {
			return errors.New("invalid recovery object identity")
		}
		s.pointers[gen] = p.Object
		s.targets[p.Target] = true
		s.keys[gen] = p.Target
		s.ids[gen] = p.NZBID
	}
	return s.cleanOrphans(entries)
}

func (s *Store) cleanOrphans(entries []os.DirEntry, keep ...string) error {
	used := make(map[string]bool)
	for _, name := range s.pointers {
		used[name] = true
	}
	for _, v := range s.views {
		if v.refs > 0 {
			used[v.object] = true
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if len(keep) > 0 && name == keep[0] {
			continue
		}
		ownedObject := len(name) == 135 && digestString(name[:64], 64) && objectName(name, name[:64])
		ownedTemp := strings.HasPrefix(name, ".tmp-") && digestString(strings.TrimPrefix(name, ".tmp-"), 32)
		if (ownedObject && !used[name]) || ownedTemp {
			if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (s *Store) readJSON(name string, value any, limit int64) error {
	f, err := s.root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return errors.New("invalid recovery metadata size")
	}
	d := json.NewDecoder(io.LimitReader(f, limit+1))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing recovery metadata")
	}
	return nil
}

func syncRoot(root *os.Root) error {
	// Windows does not expose directory fsync through os.File.Sync. File data
	// is flushed before every rename on every platform.
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func atomicJSON(root *os.Root, name string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicBytes(root, name, b)
}

func atomicBytes(root *os.Root, name string, b []byte) error {
	tmp := ".tmp-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = root.Remove(tmp) }()
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		return err
	}
	return syncRoot(root)
}

// Original NZBs from completed imports are supplied explicitly. Local copies
// are private and bounded; their only names are validated generation digests.
func (s *Store) ReadSource(gen string) ([]byte, error) {
	if !digestString(gen, 64) {
		return nil, ErrStale
	}
	f, err := s.root.Open(gen + ".source")
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrSourceRequired
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxSourceBytes {
		return nil, ErrLimit
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxSourceBytes {
		return nil, ErrLimit
	}
	return b, nil
}

// Caller holds store.mu; Start also holds the job lock, in that order.
func (s *Store) writeSource(gen string, b []byte) error {
	if !digestString(gen, 64) || len(b) == 0 || len(b) > MaxSourceBytes {
		return ErrLimit
	}
	entries, err := s.entries()
	if err != nil {
		return err
	}
	used := int64(0)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".source") && entry.Name() != gen+".source" {
			info, e := entry.Info()
			if e != nil {
				return e
			}
			used += info.Size()
			if used > 128<<20 {
				return ErrLimit
			}
		}
	}
	if int64(len(b)) > (128<<20)-used {
		return ErrLimit
	}
	return atomicBytes(s.root, gen+".source", b)
}

func (s *Store) cleanSources(generations map[string]bool) error {
	entries, err := s.entries()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		gen := strings.TrimSuffix(name, ".source")
		if strings.HasSuffix(name, ".source") && digestString(gen, 64) && !generations[gen] && s.pointers[gen] == "" {
			if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Revision(gen string) string { s.mu.Lock(); defer s.mu.Unlock(); return s.pointers[gen] }

func TargetKey(id, name string) string {
	sum := sha256.Sum256([]byte(id + "\x00" + name))
	return hex.EncodeToString(sum[:])
}
func (s *Store) HasTarget(id, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.targets[TargetKey(id, name)]
}

func validateHeader(h bundleHeader) error {
	if len(h.Blocks) > maxBlocks {
		return ErrLimit
	}
	if h.Version != 1 || !digestString(h.Generation, 64) || !digestString(h.Target, 64) || safepath.ValidateIdentifier(h.NZBID) != nil || !digestString(h.Set, 32) || !digestString(h.MD5, 32) || h.Size <= 0 || len(h.Blocks) == 0 {
		return errors.New("invalid recovery object header")
	}
	end, total := int64(0), int64(0)
	for _, b := range h.Blocks {
		if b.Length > MaxPatchBytes-total {
			return ErrLimit
		}
		if b.Offset < end || b.Length <= 0 || b.Offset > h.Size || b.Length > h.Size-b.Offset {
			return errors.New("invalid recovery block bounds")
		}
		end = b.Offset + b.Length
		total += b.Length
	}
	return nil
}

func (s *Store) open(ctx context.Context, gen string) (*view, func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, nil, errors.New("recovery store closed")
		}
		name, ok := s.pointers[gen]
		if !ok {
			s.mu.Unlock()
			return nil, func() {}, nil
		}
		v := s.views[name]
		if v == nil {
			// A small FD-only cache. Bytes are never accumulated in process memory.
			if len(s.views) >= 8 {
				for key, old := range s.views {
					if old.refs == 0 {
						_ = old.file.Close()
						delete(s.views, key)
						break
					}
				}
				if len(s.views) >= 8 {
					s.mu.Unlock()
					timer := time.NewTimer(25 * time.Millisecond)
					select {
					case <-ctx.Done():
						timer.Stop()
						return nil, nil, ctx.Err()
					case <-timer.C:
					}
					continue
				}
			}
			var err error
			v, err = s.verifyObject(name, gen)
			if err != nil {
				s.mu.Unlock()
				return nil, nil, err
			}
			if v.header.Target != s.keys[gen] || v.header.NZBID != s.ids[gen] {
				_ = v.file.Close()
				s.mu.Unlock()
				return nil, nil, errors.New("recovery pointer identity mismatch")
			}
			s.views[name] = v
		}
		v.refs++
		s.mu.Unlock()
		return v, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			v.refs--
			if v.refs == 0 && (v.retired || s.closed) {
				_ = v.file.Close()
				delete(s.views, v.object)
			}
		}, nil
	}
}

func (s *Store) verifyObject(name, gen string) (_ *view, resultErr error) {
	if !objectName(name, gen) {
		return nil, errors.New("invalid recovery object name")
	}
	f, err := s.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			_ = f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 12 || info.Size() > MaxPatchBytes+maxHeaderBytes+12 {
		return nil, errors.New("invalid recovery object size")
	}
	var prefix [12]byte
	if _, err := io.ReadFull(f, prefix[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(prefix[8:])
	if [8]byte(prefix[:8]) != objectMagic || n == 0 || n > maxHeaderBytes || int64(n) > info.Size()-12 {
		return nil, errors.New("invalid recovery object framing")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(f, b); err != nil {
		return nil, err
	}
	var h bundleHeader
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&h); err != nil {
		return nil, err
	}
	if err := validateHeader(h); err != nil || h.Generation != gen {
		return nil, errors.New("recovery object identity mismatch")
	}
	v := &view{file: f, header: h, object: name}
	pos := int64(12 + n)
	for _, r := range h.Blocks {
		v.starts = append(v.starts, pos)
		pos += r.Length
	}
	if pos != info.Size() {
		return nil, errors.New("recovery object length mismatch")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(f, 0, info.Size())); err != nil {
		return nil, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != name[65:129] {
		return nil, errors.New("recovery object checksum mismatch")
	}
	return v, nil
}

// Overlay reads patched ranges from a verified immutable object and delegates
// only the gaps. It never asks the base reader to fetch a known missing range.
func (s *Store) Overlay(ctx context.Context, gen string, size int64, p []byte, off int64, base func(context.Context, []byte, int64) (int, error)) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, errors.New("negative recovery read offset")
	}
	if off >= size {
		return 0, io.EOF
	}
	v, release, err := s.open(ctx, gen)
	if err != nil {
		return 0, err
	}
	defer release()
	if v == nil {
		return base(ctx, p, off)
	}
	if v.header.Size != size {
		return 0, errors.New("recovery file size changed")
	}
	want := min(int64(len(p)), size-off)
	done := int64(0)
	for done < want {
		if err := ctx.Err(); err != nil {
			return int(done), err
		}
		pos, end, selected := off+done, off+want, -1
		for i, r := range v.header.Blocks {
			if r.Offset+r.Length <= pos {
				continue
			}
			if r.Offset <= pos {
				selected = i
				end = min(end, r.Offset+r.Length)
			} else {
				end = min(end, r.Offset)
			}
			break
		}
		var n int
		if selected >= 0 {
			n, err = v.file.ReadAt(p[done:done+end-pos], v.starts[selected]+pos-v.header.Blocks[selected].Offset)
		} else {
			n, err = base(ctx, p[done:done+end-pos], pos)
		}
		if n < 0 || int64(n) > end-pos {
			return int(done), errors.New("invalid recovery read count")
		}
		done += int64(n)
		if int64(n) != end-pos {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return int(done), err
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return int(done), err
		}
	}
	if done < int64(len(p)) {
		return int(done), io.EOF
	}
	return int(done), nil
}

func (s *Store) Covers(gen string, offset, length int64) bool {
	return s.CoversContext(context.Background(), gen, offset, length)
}

func (s *Store) CoversContext(ctx context.Context, gen string, offset, length int64) bool {
	v, release, err := s.open(ctx, gen)
	if err != nil || v == nil {
		return false
	}
	defer release()
	if offset < 0 || length <= 0 || offset > v.header.Size || length > v.header.Size-offset {
		return false
	}
	end := offset + length
	for _, r := range v.header.Blocks {
		if r.Offset+r.Length <= offset {
			continue
		}
		if r.Offset > offset {
			return false
		}
		offset = min(end, r.Offset+r.Length)
		if offset == end {
			return true
		}
	}
	return false
}

// Publish merges compatible old blocks, writes/flushed a new object, and only
// then updates the generation pointer. validate is the source-ownership fence.
func (s *Store) Publish(ctx context.Context, gen string, size int64, set, md5sum string, blocks []Block, validate func() error, identity Identity) error {
	targetKey := TargetKey(identity.NZBID, identity.File)
	if err := ctx.Err(); err != nil {
		return err
	}
	old, release, err := s.open(ctx, gen)
	if err != nil {
		return err
	}
	defer release()
	if old != nil && (old.header.Set != set || old.header.Size != size || old.header.MD5 != md5sum) {
		return errors.New("existing recovery set differs")
	}
	type piece struct {
		r         blockRange
		data      []byte
		oldOffset int64
	}
	pieces := make(map[int64]piece)
	if old != nil {
		for i, r := range old.header.Blocks {
			pieces[r.Offset] = piece{r: r, oldOffset: old.starts[i]}
		}
	}
	for _, b := range blocks {
		pieces[b.Offset] = piece{r: blockRange{b.Offset, int64(len(b.Data))}, data: b.Data}
	}
	ordered := make([]piece, 0, len(pieces))
	for _, p := range pieces {
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].r.Offset < ordered[j].r.Offset })
	header := bundleHeader{Version: 1, Generation: gen, Target: targetKey, NZBID: identity.NZBID, Size: size, Set: set, MD5: md5sum}
	for _, p := range ordered {
		header.Blocks = append(header.Blocks, p.r)
	}
	if err := validateHeader(header); err != nil {
		return err
	}
	b, err := json.Marshal(header)
	if err != nil {
		return err
	}
	if len(b) > maxHeaderBytes {
		return ErrLimit
	}
	if err := safepath.VerifyOpenRoot(s.root, s.path); err != nil {
		return err
	}
	tmp := ".tmp-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	f, err := s.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = s.root.Remove(tmp) }()
	hash := sha256.New()
	writer := io.MultiWriter(f, hash)
	var prefix [12]byte
	copy(prefix[:8], objectMagic[:])
	binary.LittleEndian.PutUint32(prefix[8:], uint32(len(b)))
	if _, err = writer.Write(prefix[:]); err != nil {
		return err
	}
	if _, err = writer.Write(b); err != nil {
		return err
	}
	for _, p := range ordered {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.data != nil {
			_, err = writer.Write(p.data)
		} else {
			_, err = io.CopyN(writer, io.NewSectionReader(old.file, p.oldOffset, p.r.Length), p.r.Length)
		}
		if err != nil {
			return err
		}
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	object := gen + "-" + hex.EncodeToString(hash.Sum(nil)) + ".patch"
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("recovery store closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if validate != nil {
		if err := validate(); err != nil {
			return err
		}
	}
	entries, err := s.entries()
	if err != nil {
		return err
	}
	if err := s.cleanOrphans(entries, tmp); err != nil {
		return err
	}
	entries, err = s.entries()
	if err != nil {
		return err
	}
	var used int64
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".patch") {
			info, e := entry.Info()
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() {
				return errors.New("invalid recovery store object")
			}
			used += info.Size()
			if used > MaxStoreBytes {
				return ErrLimit
			}
		}
	}
	info, err := s.root.Stat(tmp)
	if err != nil {
		return err
	}
	if info.Size() > MaxStoreBytes-used {
		return ErrLimit
	}
	if _, err := s.root.Stat(object); err == nil {
		verified, e := s.verifyObject(object, gen)
		if e != nil {
			return e
		}
		_ = verified.file.Close()
	} else if errors.Is(err, os.ErrNotExist) {
		if err := s.root.Rename(tmp, object); err != nil {
			return err
		}
		if err := syncRoot(s.root); err != nil {
			return err
		}
	} else {
		return err
	}
	if s.beforePointer != nil {
		if err := s.beforePointer(); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := atomicJSON(s.root, gen+".json", pointer{Object: object, Target: targetKey, NZBID: identity.NZBID}); err != nil {
		return err
	}
	if s.afterPointer != nil {
		if err := s.afterPointer(); err != nil {
			return err
		}
	}
	previous := s.pointers[gen]
	s.pointers[gen] = object
	s.targets[targetKey] = true
	s.keys[gen] = targetKey
	s.ids[gen] = identity.NZBID
	if v := s.views[previous]; v != nil && previous != object {
		v.retired = true
		if v.refs == 0 {
			_ = v.file.Close()
			delete(s.views, previous)
		}
	}
	return nil
}

// Caller holds the publication lock and has durably cancelled this NZB's jobs.
func (s *Store) forget(id string, generations map[string]bool) error {
	for gen, nzbID := range s.ids {
		if nzbID == id {
			generations[gen] = true
		}
	}
	for gen := range generations {
		if !digestString(gen, 64) {
			return ErrStale
		}
		if err := s.root.Remove(gen + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		object := s.pointers[gen]
		delete(s.pointers, gen)
		delete(s.keys, gen)
		delete(s.ids, gen)
		if v := s.views[object]; v != nil {
			v.retired = true
			if v.refs == 0 {
				_ = v.file.Close()
				delete(s.views, object)
			}
		}
		if err := s.root.Remove(gen + ".source"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	s.targets = make(map[string]bool)
	for _, key := range s.keys {
		s.targets[key] = true
	}
	if err := syncRoot(s.root); err != nil {
		return err
	}
	entries, err := s.entries()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 135 {
			continue
		}
		gen := name[:64]
		if !generations[gen] || !objectName(name, gen) {
			continue
		}
		if v := s.views[name]; v != nil && v.refs > 0 {
			continue
		}
		if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return syncRoot(s.root)
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for key, v := range s.views {
		if v.refs == 0 {
			_ = v.file.Close()
			delete(s.views, key)
		}
	}
	return s.root.Close()
}
