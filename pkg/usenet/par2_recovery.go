package usenet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Trifocals3537/tessarr/internal/nntp"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/Trifocals3537/tessarr/pkg/usenet/fs"
	"github.com/Trifocals3537/tessarr/pkg/usenet/recovery"
)

var ErrPAR2NotConfigured = errors.New("Usenet recovery is not configured")

type recoveryHost struct{ u *Usenet }

func (h recoveryHost) Snapshot(ctx context.Context, id, name string) (recovery.Target, error) {
	if err := ctx.Err(); err != nil {
		return recovery.Target{}, err
	}
	nzb, err := h.u.nzbStorage.GetNZB(id)
	if err != nil {
		return recovery.Target{}, recovery.ErrStale
	}
	if nzb.Status != NZBStatusCompleted {
		return recovery.Target{}, recovery.ErrUnsupported
	}
	file := nzb.GetFileByName(name)
	if err := recovery.Eligible(file); err != nil {
		return recovery.Target{}, err
	}
	file.NzbID = id
	for i := range nzb.Files {
		nzb.Files[i].NzbID = id
	}
	target := recovery.Target{ID: id, Name: nzb.Name, File: *file, Files: nzb.Files, Generation: recovery.Generation(*file)}
	if nzb.Path != "" {
		path, err := validatePersistedMetadataPath(h.u.metadataDir, id, nzb.Path, nzbSourceSuffix)
		if err != nil {
			return recovery.Target{}, err
		}
		b, err := readStableMetadataContent(h.u.metadataDir, path, recovery.MaxSourceBytes)
		if err == nil {
			target.Source = b
		} // Completed-source cleanup may have already removed it.
	}
	return target, nil
}

type recoveryBuffer struct {
	bytes.Buffer
	ctx     context.Context
	consume func(int) error
}

func (b *recoveryBuffer) Write(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > recovery.MaxArticleBytes-b.Len() {
		return 0, recovery.ErrLimit
	}
	if err := b.consume(len(p)); err != nil {
		return 0, err
	}
	return b.Buffer.Write(p)
}

func (h recoveryHost) Download(ctx context.Context, id string, expected *storage.NZBArticleGeometry, consume func(int) error) ([]byte, *nntp.YencMetadata, error) {
	ctx, cancel := context.WithTimeout(nntp.WithBackground(ctx), 30*time.Second)
	defer cancel()
	var payload []byte
	var metadata *nntp.YencMetadata
	err := h.u.nntp.ExecuteWithFailover(ctx, func(conn *nntp.Connection) error {
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()
		buffer := &recoveryBuffer{ctx: ctx, consume: consume}
		n, meta, err := conn.StreamBodyWithMetadata(id, buffer)
		if err != nil {
			// A stopped writer or decoder can leave part of the body unread.
			// Only a complete article-not-found response is safe to reuse.
			if !nntp.IsArticleNotFoundError(err) {
				_ = conn.Close()
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, recovery.ErrLimit) {
				return recovery.ErrLimit
			}
			var typed *nntp.Error
			if errors.As(err, &typed) && typed.Type == nntp.ErrorTypeYencDecode {
				return &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Message: "invalid article payload", Err: err}
			}
			return err
		}
		if meta == nil || n <= 0 || n != int64(buffer.Len()) || meta.PartSize != n || meta.Size <= 0 || meta.Size > recovery.MaxReleaseBytes || meta.Begin < 1 || meta.End-meta.Begin+1 != n || meta.End > meta.Size || (expected != nil && !expected.Matches(meta.Size, meta.Begin, meta.End, meta.Part, meta.Total, n)) {
			return &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Message: "article does not match its source geometry"}
		}
		payload = buffer.Bytes()
		metadata = meta
		return nil
	})
	return payload, metadata, err
}

func (h recoveryHost) WaitIdle(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		active := false
		if h.u.fs != nil {
			h.u.fs.Range(func(_ string, entry *fsEntry) bool {
				if entry.refCount.Load() > 0 {
					active = true
					return false
				}
				return true
			})
		}
		if !active {
			return nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (h recoveryHost) Published(target recovery.Target) {
	current, err := h.Snapshot(context.Background(), target.ID, target.File.Name)
	if err != nil || current.Generation != target.Generation {
		return
	}
	key := fsKey(target.ID, target.File.Name)
	if h.u.failedFiles != nil {
		if state, ok := h.u.failedFiles.Load(key); ok {
			state.mu.Lock()
			state.retryAfter = h.u.failureClock()
			state.mu.Unlock()
		}
	}
	// Idle cached readers may remember a failed segment. Active readers retain
	// their pinned entry and see the new patch through the dynamic overlay.
	if h.u.fs != nil {
		if entry, ok := h.u.fs.Load(key); ok && entry.claimForCleanup() {
			h.u.fs.Delete(key)
			entry.cleanup()
		}
	}
}

func (u *Usenet) PAR2Service() *recovery.Service {
	if u == nil {
		return nil
	}
	return u.recovery
}

type PAR2Target struct {
	NZBID string `json:"nzb_id"`
	NZB   string `json:"nzb"`
	File  string `json:"file"`
	Size  int64  `json:"size"`
}

// Search by NZB name using the compact header codec; decode complete maps only
// for matching entries. The bounded page can be continued with its ID cursor.
func (u *Usenet) PAR2Targets(ctx context.Context, q, after string) ([]PAR2Target, string, error) {
	if u == nil {
		return nil, "", ErrPAR2NotConfigured
	}
	if u.recovery == nil {
		return nil, "", recovery.ErrUnavailable
	}
	q = strings.ToLower(strings.TrimSpace(q))
	if len(q) < 2 || len(q) > 128 {
		return []PAR2Target{}, "", nil
	}
	ids, err := u.nzbStorage.GetAllNZBIDs()
	if err != nil {
		return nil, "", err
	}
	sort.Strings(ids)
	start := sort.SearchStrings(ids, after)
	if start < len(ids) && ids[start] == after {
		start++
	}
	items := make([]PAR2Target, 0)
	end := min(start+200, len(ids))
	for i := start; i < end; i++ {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		header, err := u.nzbStorage.GetNZBHeader(ids[i])
		if err != nil {
			continue
		}
		if header.Status == NZBStatusCompleted && strings.Contains(strings.ToLower(header.Name), q) {
			nzb, err := u.nzbStorage.GetNZB(ids[i])
			if err == nil && len(nzb.Files) <= 512 {
				for _, file := range nzb.Files {
					if recovery.Eligible(&file) == nil {
						items = append(items, PAR2Target{NZBID: ids[i], NZB: nzb.Name, File: file.Name, Size: file.Size})
					}
				}
			}
		}
		// Finish a matching NZB before advancing the cursor. A pack with more
		// than 50 files must not silently lose the remainder of its targets.
		if len(items) >= 50 {
			end = i + 1
			break
		}
	}
	next := ""
	if end > start && end < len(ids) {
		next = ids[end-1]
	}
	return items, next, nil
}

type recoveredReader struct {
	base       fs.PrefetchableReaderAt
	store      *recovery.Store
	generation string
	size       int64
}

func (r *recoveredReader) ReadAt(p []byte, off int64) (int, error) {
	return r.ReadAtContext(context.Background(), p, off)
}
func (r *recoveredReader) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	return r.store.Overlay(ctx, r.generation, r.size, p, off, r.base.ReadAtContext)
}
func (r *recoveredReader) Prefetch(ctx context.Context, off, length int64) {
	r.base.Prefetch(ctx, off, length)
}

func (u *Usenet) unpatchedSamples(ctx context.Context, id string, file *storage.NZBFile, samples []string) []string {
	if u.recovery == nil {
		return samples
	}
	copyFile := *file
	copyFile.NzbID = id
	gen := recovery.Generation(copyFile)
	if u.recovery.Store().Revision(gen) == "" {
		return samples
	}
	covered := make(map[string]bool)
	for _, s := range file.Segments {
		if u.recovery.Store().CoversContext(ctx, gen, s.StartOffset, s.Bytes) {
			covered[s.MessageID] = true
		}
	}
	out := make([]string, 0, len(samples))
	for _, sample := range samples {
		if !covered[sample] {
			out = append(out, sample)
		}
	}
	return out
}

// Keep the source path derivation in the Usenet-owned configuration tree.
func (u *Usenet) recoveryPath() string { return filepath.Join(filepath.Dir(u.metadataDir), "recovery") }

var _ io.ReaderAt = (*recoveredReader)(nil)
